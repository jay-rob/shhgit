package core

import (
	"context"
	"encoding/csv"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v45/github"
	"golang.org/x/oauth2"
)

type Session struct {
	sync.Mutex

	Version          string
	Log              *Logger
	Options          *Options
	Config           *Config
	Signatures       []Signature
	Repositories     chan GitResource
	Gists            chan string
	Comments         chan string
	Context          context.Context
	Clients          chan *GitHubClientWrapper
	ExhaustedClients chan *GitHubClientWrapper
	CsvWriter        *csv.Writer
}

var (
	session     *Session
	sessionSync sync.Once
	err         error
)

func (s *Session) Start() {
	rand.Seed(time.Now().Unix())

	s.InitLogger()
	s.InitThreads()
	s.InitSignatures()
	s.InitGitHubClients()
	s.InitCsvWriter()
}

func (s *Session) InitLogger() {
	s.Log = &Logger{}
	s.Log.SetDebug(*s.Options.Debug)
	s.Log.SetSilent(*s.Options.Silent)
}

func (s *Session) InitSignatures() {
	s.Signatures = GetSignatures(s)
}

func (s *Session) InitGitHubClients() {
	if len(*s.Options.App) >= 0 {
		chanSize := *s.Options.Threads * (len(s.Config.GitHubAccessTokens) + 1)
		s.Clients = make(chan *GitHubClientWrapper, chanSize)
		s.ExhaustedClients = make(chan *GitHubClientWrapper, chanSize)

		const gitHost = "https://api.github.com"

		privatePem, err := ioutil.ReadFile("/Users/jasonroberts/workspaces/shhgit/private-key.pem")
		if err != nil {
			log.Fatalf("failed to read pem: %v", err)
		}

		itr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, 113101, privatePem)
		if err != nil {
			log.Fatalf("faild to create app transport: %v\n", err)
		}
		itr.BaseURL = gitHost

		//create git client with app transport
		client, err := github.NewEnterpriseClient(
			gitHost,
			gitHost,
			&http.Client{
				Transport: itr,
				Timeout:   time.Second * 30,
			})

		if err != nil {
			log.Fatalf("faild to create git client for app: %v\n", err)
		}

		installations, _, err := client.Apps.ListInstallations(context.Background(), &github.ListOptions{})
		if err != nil {
			log.Fatalf("failed to list installations: %v\n", err)
		}

		for _, val := range installations {
			//val.
			installID := val.GetID()
			token, _, err := client.Apps.CreateInstallationToken(
				context.Background(),
				installID, &github.InstallationTokenOptions{})
			if err != nil {
				log.Fatalf("failed to create installation token: %v\n", err)
			}
			ts := oauth2.StaticTokenSource(
				&oauth2.Token{AccessToken: token.GetToken()},
			)
			oAuthClient := oauth2.NewClient(context.Background(), ts)

			//create new git hub client with accessToken
			apiClient, err := github.NewEnterpriseClient(gitHost, gitHost, oAuthClient)
			if err != nil {
				log.Fatalf("failed to create new git client with token: %v\n", err)
			}
			for i := 0; i <= *s.Options.Threads; i++ {
				s.Clients <- &GitHubClientWrapper{apiClient, token.GetToken(), time.Now().Add(-1 * time.Second)}
			}
		}

	} else if len(*s.Options.Local) <= 0 {
		chanSize := *s.Options.Threads * (len(s.Config.GitHubAccessTokens) + 1)
		s.Clients = make(chan *GitHubClientWrapper, chanSize)
		s.ExhaustedClients = make(chan *GitHubClientWrapper, chanSize)
		for _, token := range s.Config.GitHubAccessTokens {
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(s.Context, ts)

			client := github.NewClient(tc)
			client.UserAgent = fmt.Sprintf("%s v%s", Name, Version)
			_, _, err := client.Users.Get(s.Context, "")

			if err != nil {
				if _, ok := err.(*github.ErrorResponse); ok {
					s.Log.Warn("Failed to validate token %s[..]: %s", token[:10], err)
					continue
				}
			}

			for i := 0; i <= *s.Options.Threads; i++ {
				s.Clients <- &GitHubClientWrapper{client, token, time.Now().Add(-1 * time.Second)}
			}
		}

		if len(s.Clients) < 1 {
			s.Log.Fatal("No valid GitHub tokens provided. Quitting!")
		}
	}
}

func (s *Session) GetClient() *GitHubClientWrapper {
	for {
		select {

		case client := <-s.Clients:
			s.Log.Debug("Using client with token: %s", client.Token[:10])
			return client

		case client := <-s.ExhaustedClients:
			sleepTime := time.Until(client.RateLimitedUntil)
			s.Log.Warn("All GitHub tokens exhausted/rate limited. Sleeping for %s", sleepTime.String())
			time.Sleep(sleepTime)
			s.Log.Debug("Returning client %s to pool", client.Token[:10])
			s.FreeClient(client)

		default:
			s.Log.Debug("Available Clients: %d", len(s.Clients))
			s.Log.Debug("Exhausted Clients: %d", len(s.ExhaustedClients))
			time.Sleep(time.Millisecond * 1000)
		}
	}
}

// FreeClient returns the GitHub Client to the pool of available,
// non-rate-limited channel of clients in the session
func (s *Session) FreeClient(client *GitHubClientWrapper) {
	if client.RateLimitedUntil.After(time.Now()) {
		s.ExhaustedClients <- client
	} else {
		s.Clients <- client
	}
}

func (s *Session) InitThreads() {
	if *s.Options.Threads == 0 {
		numCPUs := runtime.NumCPU()
		s.Options.Threads = &numCPUs
	}

	runtime.GOMAXPROCS(*s.Options.Threads + 1)
}

func (s *Session) InitCsvWriter() {
	if *s.Options.CsvPath == "" {
		return
	}

	writeHeader := false
	if !PathExists(*s.Options.CsvPath) {
		writeHeader = true
	}

	file, err := os.OpenFile(*s.Options.CsvPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	LogIfError("Could not create/open CSV file", err)

	s.CsvWriter = csv.NewWriter(file)

	if writeHeader {
		s.WriteToCsv([]string{"Repository name", "Signature name", "Matching file", "Matches"})
	}
}

func (s *Session) WriteToCsv(line []string) {
	if *s.Options.CsvPath == "" {
		return
	}

	s.CsvWriter.Write(line)
	s.CsvWriter.Flush()
}

func GetSession() *Session {
	sessionSync.Do(func() {
		session = &Session{
			Context:      context.Background(),
			Repositories: make(chan GitResource, 1000),
			Gists:        make(chan string, 100),
			Comments:     make(chan string, 1000),
		}

		if session.Options, err = ParseOptions(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		if session.Config, err = ParseConfig(session.Options); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		session.Start()
	})

	return session
}

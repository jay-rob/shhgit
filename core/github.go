package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v45/github"
	"golang.org/x/oauth2"
)

type GitHubClientWrapper struct {
	*github.Client
	Token            string
	RateLimitedUntil time.Time
}

const (
	perPage = 300
	sleep   = 30 * time.Second
)

// app id 114437
func GetAppInstallationClients(session *Session) {
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
		//apiClient.Apps.FindRepositoryInstallationByID()()
		repos, _, err := apiClient.Apps.ListRepos(context.Background(), &github.ListOptions{PerPage: perPage})
		//apiClient.Repositories.ListAll(context.Background(), &github.RepositoryListAllOptions{})
		if err != nil {

			session.Log.Fatal("failed to create new git client with token: %v\n", err)

		}
		for _, repo := range repos.Repositories {
			repo.GetDefaultBranch()
			session.Log.Warn("repo=%v", repo.GetURL())
			session.Repositories <- GitResource{
				Id:   *repo.ID,
				Type: GITHUB_SOURCE,
				Url:  repo.GetURL(),
				Ref:  *repo.MasterBranch,
			}
		}
	}

	//session.Log.Warn("%s", token.Repositories)
}

func GetInstallationRepositories(session *Session) {

	client := session.GetClient()
	defer session.FreeClient(client)
	repos, _, err := client.Apps.ListRepos(context.Background(), &github.ListOptions{PerPage: perPage})
	if err != nil {

		session.Log.Fatal("failed to create new git client with token: %v\n", err)

	}
	for _, repo := range repos.Repositories {
		session.Log.Warn("repo=%v", repo.GetURL())
		session.Repositories <- GitResource{
			Id:   *repo.ID,
			Type: GITHUB_SOURCE,
			Url:  repo.GetURL(),
			Ref:  repo.GetDefaultBranch(),
		}
	}

}

func getOrgRepositories(session *Session) {
	localCtx, cancel := context.WithCancel(session.Context)
	defer cancel()
	opt := &github.RepositoryListByOrgOptions{}

	//observedKeys := map[string]bool{}
	var client *GitHubClientWrapper
	for _, org := range session.Config.GithubIncludeOrgs {

		client = session.GetClient()

		repos, resp, err := client.Repositories.ListByOrg(localCtx, org, opt)
		if err != nil {
			if _, ok := err.(*github.RateLimitError); ok {
				session.Log.Warn("Token %s[..] rate limited. Reset at %s", client.Token[:10], resp.Rate.Reset)
				client.RateLimitedUntil = resp.Rate.Reset.Time
				session.FreeClient(client)
				break
			}

			if _, ok := err.(*github.AbuseRateLimitError); ok {
				session.Log.Fatal("GitHub API abused detected. Quitting...")
			}

			session.Log.Warn("Error getting GitHub events: %s... trying again", err)
		}

		for _, repo := range repos {

			session.Repositories <- GitResource{
				Id:   *repo.ID,
				Type: GITHUB_SOURCE,
				Url:  repo.GetURL(),
				Ref:  *repo.MasterBranch,
			}
		}
	}

}

func GetRepositories(session *Session) {
	localCtx, cancel := context.WithCancel(session.Context)
	defer cancel()

	observedKeys := map[string]bool{}
	var client *GitHubClientWrapper

	for c := time.Tick(sleep); ; {
		opt := &github.ListOptions{PerPage: perPage}

		for {
			if client != nil {
				session.FreeClient(client)
			}

			client = session.GetClient()
			events, resp, err := client.Activity.ListEvents(localCtx, opt)

			if err != nil {
				if _, ok := err.(*github.RateLimitError); ok {
					session.Log.Warn("Token %s[..] rate limited. Reset at %s", client.Token[:10], resp.Rate.Reset)
					client.RateLimitedUntil = resp.Rate.Reset.Time
					session.FreeClient(client)
					break
				}

				if _, ok := err.(*github.AbuseRateLimitError); ok {
					session.Log.Fatal("GitHub API abused detected. Quitting...")
				}

				session.Log.Warn("Error getting GitHub events: %s... trying again", err)
			}

			if opt.Page == 0 {
				tokenMessage := fmt.Sprintf("[?] Token %s[..] has %d/%d calls remaining.", client.Token[:10], resp.Rate.Remaining, resp.Rate.Limit)

				if resp.Rate.Remaining < 100 {
					session.Log.Warn(tokenMessage)
				} else {
					session.Log.Debug(tokenMessage)
				}
			}

			newEvents := make([]*github.Event, 0, len(events))

			// remove duplicates
			for _, e := range events {
				if observedKeys[*e.ID] {
					continue
				}

				newEvents = append(newEvents, e)
			}

			for _, e := range newEvents {
				if *e.Type == "PushEvent" {
					observedKeys[*e.ID] = true

					dst := &github.PushEvent{}
					json.Unmarshal(e.GetRawPayload(), dst)
					session.Repositories <- GitResource{
						Id:   e.GetRepo().GetID(),
						Type: GITHUB_SOURCE,
						Url:  e.GetRepo().GetURL(),
						Ref:  dst.GetRef(),
					}
				} else if *e.Type == "IssueCommentEvent" {
					observedKeys[*e.ID] = true

					dst := &github.IssueCommentEvent{}
					json.Unmarshal(e.GetRawPayload(), dst)
					session.Comments <- *dst.Comment.Body
				} else if *e.Type == "IssuesEvent" {
					observedKeys[*e.ID] = true

					dst := &github.IssuesEvent{}
					json.Unmarshal(e.GetRawPayload(), dst)
					session.Comments <- dst.Issue.GetBody()
				}
			}

			if resp.NextPage == 0 {
				break
			}

			opt.Page = resp.NextPage
			time.Sleep(5 * time.Second)
		}

		select {
		case <-c:
			continue
		case <-localCtx.Done():
			cancel()
			return
		}
	}
}

func GetGists(session *Session) {
	localCtx, cancel := context.WithCancel(session.Context)
	defer cancel()

	observedKeys := map[string]bool{}
	opt := &github.GistListOptions{}

	var client *GitHubClientWrapper
	for c := time.Tick(sleep); ; {
		if client != nil {
			session.FreeClient(client)
		}

		client = session.GetClient()
		gists, resp, err := client.Gists.ListAll(localCtx, opt)

		if err != nil {
			if _, ok := err.(*github.RateLimitError); ok {
				session.Log.Warn("Token %s[..] rate limited. Reset at %s", client.Token[:10], resp.Rate.Reset)
				client.RateLimitedUntil = resp.Rate.Reset.Time
				session.FreeClient(client)
				break
			}

			if _, ok := err.(*github.AbuseRateLimitError); ok {
				session.Log.Fatal("GitHub API abused detected. Quitting...")
			}

			session.Log.Warn("Error getting GitHub Gists: %s ... trying again", err)
		}

		newGists := make([]*github.Gist, 0, len(gists))
		for _, e := range gists {
			if observedKeys[*e.ID] {
				continue
			}

			newGists = append(newGists, e)
		}

		for _, e := range newGists {
			observedKeys[*e.ID] = true
			session.Gists <- *e.GitPullURL
		}

		opt.Since = time.Now()

		select {
		case <-c:
			continue
		case <-localCtx.Done():
			cancel()
			return
		}
	}
}

func GetRepository(session *Session, id int64) (*github.Repository, error) {
	client := session.GetClient()
	defer session.FreeClient(client)

	if len(*session.Options.App) > 0 {
		repo, resp, err := client.Repositories.GetByID(session.Context, id)

		if err != nil {
			return nil, err
		}

		if resp.Rate.Remaining <= 1 {
			session.Log.Warn("Token %s[..] rate limited. Reset at %s", client.Token[:10], resp.Rate.Reset)
			client.RateLimitedUntil = resp.Rate.Reset.Time
		}
		return repo, nil
	}
	repo, resp, err := client.Repositories.GetByID(session.Context, id)

	if err != nil {
		return nil, err
	}

	if resp.Rate.Remaining <= 1 {
		session.Log.Warn("Token %s[..] rate limited. Reset at %s", client.Token[:10], resp.Rate.Reset)
		client.RateLimitedUntil = resp.Rate.Reset.Time
	}
	return repo, nil
}

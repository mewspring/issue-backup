// The issue-backup stores a backup of GitHub issues in JSON format.
//
// Usage:
//
//    issue-backup [OPTION]...
//
// Flags:
//
//   -owner string
//         owner name (GitHub user or organization)
//   -q    suppress non-error messages
//   -repo string
//         repository name
//   -token string
//         GitHub OAuth personal access token
//
// Example:
//
//    issue-backup -owner USER -repo REPO -token ACCESS_TOKEN
//
// To create a personal access token on GitHub visit https://github.com/settings/tokens
//
// If the environment variable ISSUE_BACKUP_GITHUB_TOKEN is set, the access
// token will be read from there.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/go-github/v32/github"
	"github.com/mewkiz/pkg/jsonutil"
	"github.com/mewkiz/pkg/term"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
)

var (
	// dbg is a logger with the "issue-backup:" prefix which logs debug messages
	// to standard error.
	dbg = log.New(os.Stderr, term.CyanBold("issue-backup:")+" ", 0)
	// warn is a logger with the "issue-backup:" prefix which logs warning
	// messages to standard error.
	warn = log.New(os.Stderr, term.RedBold("issue-backup:")+" ", 0)
)

const issueBackupTokenEnvName = "ISSUE_BACKUP_GITHUB_TOKEN"

const use = `
Usage:

	issue-backup [OPTION]...

Flags:
`

const example = `
Example:

	issue-backup -owner USER -repo REPO -token ACCESS_TOKEN

To create a personal access token on GitHub visit https://github.com/settings/tokens

If the environment variable ` + issueBackupTokenEnvName + ` is set, the access token will be read from there.
`

func usage() {
	fmt.Fprintln(os.Stderr, use[1:])
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, example)
}

func main() {
	// Parse command line arguments.
	var (
		// Owner name (GitHub user or organization).
		ownerName string
		// Suppress non-error messages.
		quiet bool
		// Repository name.
		repoName string
		// GitHub OAuth personal access token.
		token string
	)
	flag.StringVar(&ownerName, "owner", "", "owner name (GitHub user or organization)")
	flag.BoolVar(&quiet, "q", false, "suppress non-error messages")
	flag.StringVar(&repoName, "repo", "", "repository name")
	flag.StringVar(&token, "token", "", "GitHub OAuth personal access token")
	flag.Usage = usage
	flag.Parse()
	// Sanity check of command line flags.
	if len(ownerName) == 0 {
		log.Println("owner name not specified; see -owner flag")
		flag.Usage()
		os.Exit(1)
	}
	if len(repoName) == 0 {
		log.Println("repository name not specified; see -repo flag")
		flag.Usage()
		os.Exit(1)
	}
	if envToken, ok := os.LookupEnv(issueBackupTokenEnvName); ok {
		dbg.Printf("using OAuth token from %s environment variable", issueBackupTokenEnvName)
		token = envToken
	}
	if len(token) == 0 {
		warn.Printf("OAuth token not specified; use -token flag or set %s environment variable", issueBackupTokenEnvName)
	}
	// Mute debug messages if `-q` is set.
	if quiet {
		dbg.SetOutput(ioutil.Discard)
	}
	// Locate forks with divergent commits.
	if err := backupIssues(ownerName, repoName, token); err != nil {
		log.Fatalf("%+v", err)
	}
}

// backupIssues creates a backup of all issues of the given owner/repo.
func backupIssues(ownerName, repoName, token string) error {
	c := newClient(token)
	// Get issues.
	issues, err := c.getIssues(ownerName, repoName)
	if err != nil {
		return errors.WithStack(err)
	}
	for _, issue := range issues {
		dbg.Printf("issue #%d", issue.GetNumber())
		if err := jsonutil.Write(os.Stdout, issue); err != nil {
			return errors.WithStack(err)
		}
		fmt.Println()
		if issue.GetComments() > 0 {
			dbg.Printf("%d comments of issue #%d", issue.GetComments(), issue.GetNumber())
			comments, err := c.getIssueComments(ownerName, repoName, issue.GetNumber())
			if err != nil {
				return errors.WithStack(err)
			}
			if err := jsonutil.Write(os.Stdout, comments); err != nil {
				return errors.WithStack(err)
			}
			fmt.Println()
		}
	}
	return nil
}

// Client is an OAuth authenticated GitHub client.
type Client struct {
	ctx    context.Context
	client *github.Client
}

// newClient returns a GitHub client authenticated with the given OAuth token.
func newClient(token string) *Client {
	ctx := context.Background()
	var tc *http.Client
	// Use personal OAuth access token if specified.
	if len(token) > 0 {
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: token},
		)
		tc = oauth2.NewClient(ctx, ts)
	}
	client := github.NewClient(tc)
	return &Client{
		ctx:    ctx,
		client: client,
	}
}

// getIssues returns the issues of the given owner/repo.
func (c *Client) getIssues(ownerName, repoName string) ([]*github.Issue, error) {
	opt := &github.IssueListByRepoOptions{
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}
	// get commits from all pages.
	var allIssues []*github.Issue
	page := 1
	for {
		issues, resp, err := c.client.Issues.ListByRepo(c.ctx, ownerName, repoName, opt)
		if err != nil {
			for waitForRateLimitReset(err) {
				// try again after rate limit resets.
				issues, resp, err = c.client.Issues.ListByRepo(c.ctx, ownerName, repoName, opt)
			}
			if err != nil {
				warn.Printf("unable to get issues of %s:%s (page %d); %v", ownerName, repoName, page, err)
				break // return partial results
			}
		}
		allIssues = append(allIssues, issues...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
		page++
	}
	return allIssues, nil
}

// getIssueComments returns the comments for the specified issue number of the
// given owner/repo.
func (c *Client) getIssueComments(ownerName, repoName string, issueNumber int) ([]*github.IssueComment, error) {
	opt := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}
	// get commits from all pages.
	var allComments []*github.IssueComment
	page := 1
	for {
		comments, resp, err := c.client.Issues.ListComments(c.ctx, ownerName, repoName, issueNumber, opt)
		if err != nil {
			for waitForRateLimitReset(err) {
				// try again after rate limit resets.
				comments, resp, err = c.client.Issues.ListComments(c.ctx, ownerName, repoName, issueNumber, opt)
			}
			if err != nil {
				warn.Printf("unable to get comments of %s:%s for issue #%d (page %d); %v", ownerName, repoName, issueNumber, page, err)
				break // return partial results
			}
		}
		allComments = append(allComments, comments...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
		page++
	}
	return allComments, nil
}

// ### [ Helper functions ] ####################################################

// waitForRateLimitReset waits until the rate limit resets. The boolean return
// value indicates whether the given error is a GitHub rate limit error.
func waitForRateLimitReset(err error) bool {
	e, ok := err.(*github.RateLimitError)
	if !ok {
		return false
	}
	delta := time.Until(e.Rate.Reset.Time)
	dbg.Printf("rate limit hit; sleeping for %v before retrying", delta)
	time.Sleep(delta)
	return true
}

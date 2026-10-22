// Package github wraps the GitHub REST API behind an interface so the rest of
// the application can be tested without a network, and so the set of
// operations Shuriken is capable of is visible in one place.
//
// There is deliberately no method here that merges, pushes, approves or
// requests changes. The only writes are comments, reactions and COMMENT
// reviews.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v92/github"

	"github.com/sos-academy/shuriken/internal/diff"
)

// PullRequest is the metadata a review needs.
type PullRequest struct {
	Number            int
	GithubID          int64
	Title             string
	Body              string
	URL               string
	HeadSHA           string
	BaseRef           string
	Draft             bool
	Author            string
	AuthorID          int64
	AuthorIsBot       bool
	AuthorAssociation string
	Additions         int
	Deletions         int
	ChangedFiles      int
}

// ExistingComment is one of Shuriken's own earlier review comments, used to
// avoid saying the same thing twice on a pull request.
type ExistingComment struct {
	ID   int64
	Path string
	Line int
	Body string
}

// ReviewComment is one inline comment to post.
type ReviewComment struct {
	Path string
	Line int
	Side string
	Body string
}

// SubmittedReview is the result of posting a review.
type SubmittedReview struct {
	ID  int64
	URL string
}

// Client is the GitHub surface Shuriken uses.
type Client interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*PullRequest, error)
	ListPullRequestFiles(ctx context.Context, owner, repo string, number int) ([]diff.File, error)
	// GetFileContents reads a file at an exact ref. Used by the model's
	// read_file tool when the diff alone is not enough context.
	GetFileContents(ctx context.Context, owner, repo, path, ref string) (string, error)
	// ListOwnReviewComments returns review comments previously left by the
	// authenticated app on a pull request.
	ListOwnReviewComments(ctx context.Context, owner, repo string, number int, self string) ([]ExistingComment, error)
	// CreateReview submits exactly one COMMENT review. It never approves and
	// never requests changes.
	CreateReview(ctx context.Context, owner, repo string, number int, commitSHA, body string, comments []ReviewComment) (*SubmittedReview, error)
	CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (int64, error)
	// AddIssueCommentReaction reacts to an issue or pull request conversation
	// comment. A reaction is the least chatty acknowledgement available.
	AddIssueCommentReaction(ctx context.Context, owner, repo string, commentID int64, reaction string) error
	// AddReviewCommentReaction reacts to an inline pull request review comment,
	// which is a different endpoint from conversation comments.
	AddReviewCommentReaction(ctx context.Context, owner, repo string, commentID int64, reaction string) error
}

// ErrNotFound reports a missing resource, such as an optional conventions file.
var ErrNotFound = errors.New("github: not found")

// AppClientFactory mints per-installation clients for a GitHub App.
type AppClientFactory struct {
	appID      int64
	privateKey []byte
	base       *http.Client
}

// NewAppClientFactory validates the App credentials once, up front, so a bad
// private key fails at boot instead of on the first webhook.
func NewAppClientFactory(appID int64, privateKeyPEM []byte) (*AppClientFactory, error) {
	if _, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, privateKeyPEM); err != nil {
		return nil, fmt.Errorf("invalid GitHub App private key: %w", err)
	}
	return &AppClientFactory{
		appID:      appID,
		privateKey: privateKeyPEM,
		base:       &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// InstallationFor resolves the installation id for one repository, as the App
// itself. Webhook deliveries carry the id, so only a command line needs this.
func (f *AppClientFactory) InstallationFor(ctx context.Context, owner, repo string) (int64, error) {
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, f.appID, f.privateKey)
	if err != nil {
		return 0, fmt.Errorf("app transport: %w", err)
	}
	api, err := gh.NewClient(gh.WithHTTPClient(&http.Client{Transport: atr, Timeout: 30 * time.Second}))
	if err != nil {
		return 0, fmt.Errorf("github client: %w", err)
	}
	inst, resp, err := api.Apps.GetRepositoryInstallation(ctx, owner, repo)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return 0, fmt.Errorf("%w: the App is not installed on %s/%s", ErrNotFound, owner, repo)
		}
		return 0, fmt.Errorf("looking up installation for %s/%s: %w", owner, repo, err)
	}
	return inst.GetID(), nil
}

// For returns a Client authenticated as the given installation.
func (f *AppClientFactory) For(installationID int64) (Client, error) {
	itr, err := ghinstallation.New(f.base.Transport, f.appID, installationID, f.privateKey)
	if err != nil {
		return nil, fmt.Errorf("installation transport: %w", err)
	}
	httpClient := &http.Client{Transport: itr, Timeout: 60 * time.Second}
	api, err := gh.NewClient(gh.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	return &restClient{api: api}, nil
}

type restClient struct {
	api *gh.Client
}

func (c *restClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (*PullRequest, error) {
	pr, _, err := c.api.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	return convertPR(pr), nil
}

func convertPR(pr *gh.PullRequest) *PullRequest {
	out := &PullRequest{
		Number:            pr.GetNumber(),
		GithubID:          pr.GetID(),
		Title:             pr.GetTitle(),
		Body:              pr.GetBody(),
		URL:               pr.GetHTMLURL(),
		HeadSHA:           pr.GetHead().GetSHA(),
		BaseRef:           pr.GetBase().GetRef(),
		Draft:             pr.GetDraft(),
		AuthorAssociation: pr.GetAuthorAssociation(),
		Additions:         pr.GetAdditions(),
		Deletions:         pr.GetDeletions(),
		ChangedFiles:      pr.GetChangedFiles(),
	}
	if u := pr.GetUser(); u != nil {
		out.Author = u.GetLogin()
		out.AuthorID = u.GetID()
		out.AuthorIsBot = IsBotActor(u.GetType(), u.GetLogin())
	}
	return out
}

func (c *restClient) ListPullRequestFiles(ctx context.Context, owner, repo string, number int) ([]diff.File, error) {
	var out []diff.File
	opt := &gh.ListOptions{PerPage: 100}
	for {
		files, resp, err := c.api.PullRequests.ListFiles(ctx, owner, repo, number, opt)
		if err != nil {
			return nil, fmt.Errorf("list files %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, f := range files {
			out = append(out, diff.File{
				Path:      f.GetFilename(),
				PrevPath:  f.GetPreviousFilename(),
				Status:    f.GetStatus(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
				Patch:     f.GetPatch(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

func (c *restClient) GetFileContents(ctx context.Context, owner, repo, path, ref string) (string, error) {
	file, _, resp, err := c.api.Repositories.GetContents(
		ctx, owner, repo, path, &gh.RepositoryContentGetOptions{Ref: ref},
	)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("%w: %s@%s", ErrNotFound, path, ref)
		}
		return "", fmt.Errorf("get contents %s: %w", path, err)
	}
	if file == nil {
		// A directory, not a file.
		return "", fmt.Errorf("%w: %s is not a file", ErrNotFound, path)
	}
	content, err := file.GetContent()
	if err != nil {
		return "", fmt.Errorf("decode contents %s: %w", path, err)
	}
	return content, nil
}

func (c *restClient) ListOwnReviewComments(ctx context.Context, owner, repo string, number int, self string) ([]ExistingComment, error) {
	var out []ExistingComment
	opt := &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.api.PullRequests.ListComments(ctx, owner, repo, number, opt)
		if err != nil {
			return nil, fmt.Errorf("list review comments %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, cm := range comments {
			login := cm.GetUser().GetLogin()
			if self != "" && !sameActor(login, self) {
				continue
			}
			out = append(out, ExistingComment{
				ID:   cm.GetID(),
				Path: cm.GetPath(),
				Line: cm.GetLine(),
				Body: cm.GetBody(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

func (c *restClient) CreateReview(ctx context.Context, owner, repo string, number int, commitSHA, body string, comments []ReviewComment) (*SubmittedReview, error) {
	drafts := make([]*gh.DraftReviewComment, 0, len(comments))
	for _, cm := range comments {
		drafts = append(drafts, &gh.DraftReviewComment{
			Path: new(cm.Path),
			Line: new(cm.Line),
			Side: new(cm.Side),
			Body: new(cm.Body),
		})
	}

	// event COMMENT only. Shuriken is a mentor, not a gatekeeper: it must never
	// be able to approve or block a pull request.
	req := &gh.PullRequestReviewRequest{
		CommitID: new(commitSHA),
		Body:     new(body),
		Event:    new("COMMENT"),
		Comments: drafts,
	}
	review, _, err := c.api.PullRequests.CreateReview(ctx, owner, repo, number, req)
	if err != nil {
		return nil, fmt.Errorf("create review %s/%s#%d: %w", owner, repo, number, err)
	}
	return &SubmittedReview{ID: review.GetID(), URL: review.GetHTMLURL()}, nil
}

func (c *restClient) CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	cm, _, err := c.api.Issues.CreateComment(ctx, owner, repo, number, gh.IssueCommentRequest{Body: body})
	if err != nil {
		return 0, fmt.Errorf("create comment %s/%s#%d: %w", owner, repo, number, err)
	}
	return cm.GetID(), nil
}

func (c *restClient) AddIssueCommentReaction(ctx context.Context, owner, repo string, commentID int64, reaction string) error {
	_, _, err := c.api.Reactions.CreateIssueCommentReaction(ctx, owner, repo, commentID, reaction)
	if err != nil {
		return fmt.Errorf("add reaction to issue comment %d: %w", commentID, err)
	}
	return nil
}

func (c *restClient) AddReviewCommentReaction(ctx context.Context, owner, repo string, commentID int64, reaction string) error {
	_, _, err := c.api.Reactions.CreatePullRequestCommentReaction(ctx, owner, repo, commentID, reaction)
	if err != nil {
		return fmt.Errorf("add reaction to review comment %d: %w", commentID, err)
	}
	return nil
}

// IsBotActor reports whether a webhook sender is a bot. Both signals are
// checked because the App's own comments arrive with a "[bot]" login suffix.
func IsBotActor(userType, login string) bool {
	if strings.EqualFold(userType, "Bot") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(login), "[bot]")
}

// sameActor compares a comment author against our configured handle, allowing
// for the "[bot]" suffix GitHub adds to App logins.
func sameActor(login, handle string) bool {
	l := strings.ToLower(strings.TrimSuffix(strings.ToLower(login), "[bot]"))
	return l == strings.ToLower(strings.TrimSuffix(strings.ToLower(handle), "[bot]"))
}

// MaintainerAssociations are the author_association values Shuriken treats as
// maintainer. Using the association that already arrives in the webhook
// payload avoids asking for the Administration permission.
var MaintainerAssociations = map[string]bool{
	"OWNER":        true,
	"MEMBER":       true,
	"COLLABORATOR": true,
}

// IsMaintainer reports whether an author_association grants review authority
// over someone else's pull request.
func IsMaintainer(association string) bool {
	return MaintainerAssociations[strings.ToUpper(association)]
}

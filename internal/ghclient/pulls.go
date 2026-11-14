package ghclient

import (
	"context"
	"errors"
	"fmt"
)

type prRepo struct {
	FullName string `json:"full_name"`
}

// PullRequest is a PR from the pulls API.
type PullRequest struct {
	Number   int     `json:"number"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
	Head     struct {
		Ref  string  `json:"ref"`
		SHA  string  `json:"sha"`
		Repo *prRepo `json:"repo"` // nil once a fork is deleted
	} `json:"head"`
	Base struct {
		Ref  string  `json:"ref"`
		Repo *prRepo `json:"repo"`
	} `json:"base"`
	UpdatedAt string `json:"updated_at"`
}

// Fork says whether the PR's head lives outside the repo it targets.
func (p PullRequest) Fork() bool {
	return p.Head.Repo == nil || p.Base.Repo == nil || p.Head.Repo.FullName != p.Base.Repo.FullName
}

type Review struct {
	ID   int64 `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Body        string `json:"body"`
	State       string `json:"state"`
	CommitID    string `json:"commit_id"`
	SubmittedAt string `json:"submitted_at"`
}

// ReviewComment is a line comment. Line is nil once GitHub can't place it on the current diff.
type ReviewComment struct {
	ID        int64 `json:"id"`
	InReplyTo int64 `json:"in_reply_to_id"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Path             string `json:"path"`
	Line             *int   `json:"line"`
	OriginalLine     *int   `json:"original_line"`
	Side             string `json:"side"`
	OriginalCommitID string `json:"original_commit_id"`
	Body             string `json:"body"`
	PullRequestURL   string `json:"pull_request_url"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type LineComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// NewReview is a review to post. Event is COMMENT, APPROVE or REQUEST_CHANGES.
type NewReview struct {
	CommitID string        `json:"commit_id"`
	Event    string        `json:"event"`
	Body     string        `json:"body"`
	Comments []LineComment `json:"comments,omitempty"`
}

var errStop = errors.New("stop paging")

// PullsSince pages through PRs in any state updated at or after since (all of them if ""), most
// recently updated first, because the pulls listing has no since filter. Its first page is the
// same until a PR changes, so an idle repo costs one 304.
func (a *App) PullsSince(ctx context.Context, owner, repo, since string, f func([]PullRequest) error) error {
	err := pages(ctx, a, a.repoURL(owner, repo)+"/pulls?state=all&sort=updated&direction=desc&per_page=100", func(page []PullRequest) error {
		n := len(page)
		for n > 0 && page[n-1].UpdatedAt < since {
			n--
		}
		if err := f(page[:n]); err != nil {
			return err
		}
		if n < len(page) {
			return errStop
		}
		return nil
	})
	if errors.Is(err, errStop) {
		return nil
	}
	return err
}

func (a *App) PR(ctx context.Context, owner, repo string, n int) (PullRequest, error) {
	var p PullRequest
	_, err := a.get(ctx, fmt.Sprintf("%s/pulls/%d", a.repoURL(owner, repo), n), &p)
	return p, err
}

func (a *App) Reviews(ctx context.Context, owner, repo string, n int) ([]Review, error) {
	var out []Review
	err := pages(ctx, a, fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", a.repoURL(owner, repo), n),
		func(page []Review) error { out = append(out, page...); return nil })
	return out, err
}

// ReviewCommentsSince pages through the repo's line comments like IssuesSince.
func (a *App) ReviewCommentsSince(ctx context.Context, owner, repo, since string, f func([]ReviewComment) error) error {
	return pages(ctx, a, a.repoURL(owner, repo)+"/pulls/comments?sort=updated&direction=asc&per_page=100"+sinceParam(since), f)
}

// ReviewComments lists a PR's line comments updated since, or only those of one review if reviewID isn't 0.
func (a *App) ReviewComments(ctx context.Context, owner, repo string, n int, reviewID int64, since string) ([]ReviewComment, error) {
	u := fmt.Sprintf("%s/pulls/%d/comments?per_page=100%s", a.repoURL(owner, repo), n, sinceParam(since))
	if reviewID != 0 {
		u = fmt.Sprintf("%s/pulls/%d/reviews/%d/comments?per_page=100", a.repoURL(owner, repo), n, reviewID)
	}
	var out []ReviewComment
	err := pages(ctx, a, u, func(page []ReviewComment) error { out = append(out, page...); return nil })
	return out, err
}

func (a *App) CreateReview(ctx context.Context, owner, repo string, n int, user string, in NewReview) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := a.sendAs(ctx, user, "POST", fmt.Sprintf("%s/pulls/%d/reviews", a.repoURL(owner, repo), n), in, &out)
	return out.ID, err
}

// ReplyToComment replies in the thread of line comment parent.
func (a *App) ReplyToComment(ctx context.Context, owner, repo string, n int, user string, parent int64, body string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := a.sendAs(ctx, user, "POST", fmt.Sprintf("%s/pulls/%d/comments/%d/replies", a.repoURL(owner, repo), n, parent), map[string]string{"body": body}, &out)
	return out.ID, err
}

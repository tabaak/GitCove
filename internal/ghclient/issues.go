package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Issue is an issue or PR from the issues API (which returns both).
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct{} `json:"pull_request"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
}

// Comment is a conversation comment on an issue or PR.
type Comment struct {
	ID       int64  `json:"id"`
	IssueURL string `json:"issue_url"`
	Body     string `json:"body"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type NewIssue struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

func (a *App) repoURL(owner, repo string) string {
	return fmt.Sprintf("%s/repos/%s/%s", a.API, owner, repo)
}

// pages GETs u and each following page, decoding each into a fresh T and passing it to f.
func pages[T any](ctx context.Context, a *App, u string, f func(T) error) error {
	for u != "" {
		var page T
		next, err := a.get(ctx, u, &page)
		if err != nil {
			return err
		}
		if err := f(page); err != nil {
			return err
		}
		u = next
	}
	return nil
}

func sinceParam(since string) string {
	if since == "" {
		return ""
	}
	return "&since=" + url.QueryEscape(since)
}

// IssuesSince pages through issues and PRs updated at or after since (all of them if ""), least
// recently updated first, so a caller saving a cursor per page can resume. With since fixed, an
// unchanged first page comes back as a 304, which GitHub doesn't count against the limit.
func (a *App) IssuesSince(ctx context.Context, owner, repo, since string, f func([]Issue) error) error {
	return pages(ctx, a, a.repoURL(owner, repo)+"/issues?state=all&sort=updated&direction=asc&per_page=100"+sinceParam(since), f)
}

// CommentsSince pages through the repo's issue and PR comments like IssuesSince.
func (a *App) CommentsSince(ctx context.Context, owner, repo, since string, f func([]Comment) error) error {
	return pages(ctx, a, a.repoURL(owner, repo)+"/issues/comments?sort=updated&direction=asc&per_page=100"+sinceParam(since), f)
}

func (a *App) Labels(ctx context.Context, owner, repo string) ([]string, error) {
	var out []string
	err := pages(ctx, a, a.repoURL(owner, repo)+"/labels?per_page=100", func(page []struct{ Name string }) error {
		for _, l := range page {
			out = append(out, l.Name)
		}
		return nil
	})
	return out, err
}

func (a *App) Issue(ctx context.Context, owner, repo string, n int) (Issue, error) {
	var i Issue
	_, err := a.get(ctx, fmt.Sprintf("%s/issues/%d", a.repoURL(owner, repo), n), &i)
	return i, err
}

// IssuesBy lists issues and PRs in any state opened by creator and updated since: the filtered
// listing the marker lookup uses (spike M), never search.
func (a *App) IssuesBy(ctx context.Context, owner, repo, creator, since string) ([]Issue, error) {
	var out []Issue
	err := pages(ctx, a, a.repoURL(owner, repo)+"/issues?state=all&per_page=100&creator="+url.QueryEscape(creator)+sinceParam(since),
		func(page []Issue) error { out = append(out, page...); return nil })
	return out, err
}

// IssueComments lists an issue's comments updated since.
func (a *App) IssueComments(ctx context.Context, owner, repo string, n int, since string) ([]Comment, error) {
	var out []Comment
	err := pages(ctx, a, fmt.Sprintf("%s/issues/%d/comments?per_page=100%s", a.repoURL(owner, repo), n, sinceParam(since)),
		func(page []Comment) error { out = append(out, page...); return nil })
	return out, err
}

// AppLogin is the App's bot login, "<slug>[bot]", the author of what it posts.
func (a *App) AppLogin(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.login != "" {
		return a.login, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := a.call(ctx, "GET", a.API+"/app", jwt, &app); err != nil {
		return "", err
	}
	a.login = app.Slug + "[bot]"
	return a.login, nil
}

// The writes below act as the person holding user token user, or as the App if user is "".

func (a *App) CreateIssue(ctx context.Context, owner, repo, user string, in NewIssue) (int, error) {
	var out struct {
		Number int `json:"number"`
	}
	err := a.sendAs(ctx, user, "POST", a.repoURL(owner, repo)+"/issues", in, &out)
	return out.Number, err
}

func (a *App) CreateComment(ctx context.Context, owner, repo string, n int, user, body string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := a.sendAs(ctx, user, "POST", fmt.Sprintf("%s/issues/%d/comments", a.repoURL(owner, repo), n), map[string]string{"body": body}, &out)
	return out.ID, err
}

// EditIssue sets fields of an issue or PR: title, body or state.
func (a *App) EditIssue(ctx context.Context, owner, repo string, n int, user string, fields map[string]string) error {
	return a.sendAs(ctx, user, "PATCH", fmt.Sprintf("%s/issues/%d", a.repoURL(owner, repo), n), fields, nil)
}

func (a *App) AddLabel(ctx context.Context, owner, repo string, n int, user, label string) error {
	return a.sendAs(ctx, user, "POST", fmt.Sprintf("%s/issues/%d/labels", a.repoURL(owner, repo), n), map[string][]string{"labels": {label}}, nil)
}

func (a *App) RemoveLabel(ctx context.Context, owner, repo string, n int, user, label string) error {
	return a.sendAs(ctx, user, "DELETE", fmt.Sprintf("%s/issues/%d/labels/%s", a.repoURL(owner, repo), n, url.PathEscape(label)), nil, nil)
}

// IsRateLimited says whether err is GitHub refusing a request for a rate limit, primary or secondary.
func IsRateLimited(err error) bool {
	var se *StatusError
	return errors.Is(err, ErrRateLimited) || errors.As(err, &se) &&
		(se.Status == http.StatusTooManyRequests || se.Status == http.StatusForbidden && strings.Contains(strings.ToLower(se.Message), "rate limit"))
}

// Budget limits content writes (design 8.9). No header reports GitHub's content budget, so
// GitCove keeps its own count.
type Budget struct {
	Gap                time.Duration // at least this long between writes
	PerMinute, PerHour int
}

var DefaultBudget = Budget{Gap: time.Second, PerMinute: 60, PerHour: 450}

// Estimate is about how long n writes take within the budget.
func (b Budget) Estimate(n int) time.Duration {
	if n > b.PerHour {
		return time.Duration(n) * time.Hour / time.Duration(b.PerHour)
	}
	return max(time.Duration(n)*b.Gap, time.Duration(n)*time.Minute/time.Duration(b.PerMinute))
}

// next is the earliest time a write may start, given the writes of the last hour, oldest first.
func (b Budget) next(hist []time.Time, now time.Time) time.Time {
	t := now
	if n := len(hist); n > 0 {
		t = later(t, hist[n-1].Add(b.Gap))
	}
	if n := len(hist); n >= b.PerMinute {
		t = later(t, hist[n-b.PerMinute].Add(time.Minute))
	}
	if n := len(hist); n >= b.PerHour {
		t = later(t, hist[n-b.PerHour].Add(time.Hour))
	}
	return t
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// waitWrite blocks until the budget allows another write, then counts it. Writes are serialised.
// shortcut: the count lives in memory, so a restart forgets the last hour's writes; persist it if restarts during replay become common.
func (a *App) waitWrite(ctx context.Context) error {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	b := a.Budget
	if b == (Budget{}) {
		b = DefaultBudget
	}
	for {
		now := time.Now()
		for len(a.writes) > 0 && now.Sub(a.writes[0]) >= time.Hour {
			a.writes = a.writes[1:]
		}
		at := b.next(a.writes, now)
		if !at.After(now) {
			a.writes = append(a.writes, now)
			return nil
		}
		t := time.NewTimer(at.Sub(now))
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}

// sendAs does a write within the budget, as the App (user "") or as a user, and decodes the response into v if not nil.
func (a *App) sendAs(ctx context.Context, user, method, url string, in, v any) error {
	if err := a.waitWrite(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	tok := user
	if tok == "" {
		var err error
		if tok, err = a.GitToken(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrNotSent, err)
		}
	}
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	_, resp, err := a.do(ctx, method, url, tok, "", body)
	if err != nil {
		if user == "" {
			a.dropTokenOn401(err)
		}
		return err
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(resp, v)
}

// UserToken is a person's GitHub user access token and its refresh token (spike U: 8 hours and about 181 days).
type UserToken struct {
	Access, Refresh               string
	AccessExpires, RefreshExpires time.Time
}

// OAuthError is an error answer from GitHub's OAuth endpoints, e.g. authorization_pending or bad_refresh_token.
type OAuthError struct{ Code, Description string }

func (e *OAuthError) Error() string { return "github oauth: " + e.Code + ": " + e.Description }

type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// DeviceCode starts GitHub's device flow for the App's client ID.
func (a *App) DeviceCode(ctx context.Context) (DeviceCode, error) {
	var dc DeviceCode
	err := a.oauth(ctx, "/login/device/code", url.Values{"client_id": {a.ClientID}}, &dc)
	return dc, err
}

// PollDevice asks once whether the person entered the device code. Until they do, it returns an
// *OAuthError with code authorization_pending (or slow_down).
func (a *App) PollDevice(ctx context.Context, deviceCode string) (UserToken, error) {
	return a.userToken(ctx, url.Values{"client_id": {a.ClientID}, "device_code": {deviceCode},
		"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}})
}

// RefreshUser trades a refresh token for new tokens. GitHub rotates the refresh token: the old one stops working at once.
func (a *App) RefreshUser(ctx context.Context, refresh string) (UserToken, error) {
	return a.userToken(ctx, url.Values{"client_id": {a.ClientID}, "client_secret": {a.ClientSecret},
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
}

func (a *App) userToken(ctx context.Context, form url.Values) (UserToken, error) {
	var t struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int    `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	}
	if err := a.oauth(ctx, "/login/oauth/access_token", form, &t); err != nil {
		return UserToken{}, err
	}
	if t.AccessToken == "" || t.RefreshToken == "" {
		return UserToken{}, errors.New("github oauth: no access or refresh token (is token expiration on for the App?)")
	}
	now := time.Now()
	return UserToken{t.AccessToken, t.RefreshToken,
		now.Add(time.Duration(t.ExpiresIn) * time.Second), now.Add(time.Duration(t.RefreshTokenExpiresIn) * time.Second)}, nil
}

// oauth POSTs a form to GitHub's web host. Errors come back as 200 with an "error" field.
func (a *App) oauth(ctx context.Context, path string, form url.Values, v any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", a.Web+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var oe struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &oe) == nil && oe.Error != "" {
		return &OAuthError{oe.Error, oe.Description}
	}
	if resp.StatusCode/100 != 2 {
		return &StatusError{resp.StatusCode, string(bytes.TrimSpace(body))}
	}
	return json.Unmarshal(body, v)
}

// UserLogin returns the login of the person a user access token belongs to.
func (a *App) UserLogin(ctx context.Context, access string) (string, error) {
	_, body, err := a.do(ctx, "GET", a.API+"/user", access, "", nil)
	if err != nil {
		return "", err
	}
	var u struct {
		Login string `json:"login"`
	}
	return u.Login, json.Unmarshal(body, &u)
}

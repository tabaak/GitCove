// Package ghclient is GitCove's only way to call the GitHub API.
package ghclient

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Client is the GitHub API as GitCove uses it.
type Client interface {
	// Repos lists the repos the App installation can see.
	Repos(ctx context.Context) ([]Repo, error)
	// GitToken returns an installation token for Git over HTTPS.
	GitToken(ctx context.Context) (string, error)
	// Probe makes one cheap authenticated API call, for health checks.
	Probe(ctx context.Context) error
}

type Repo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// ErrRateLimited is returned when a request would have to wait past the context deadline for GitHub's limit to reset.
var ErrRateLimited = errors.New("github rate limit")

// StatusError is a non-2xx response.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string { return fmt.Sprintf("github: %d %s", e.Status, e.Message) }

// App authenticates as a GitHub App installation on one organisation.
type App struct {
	API   string // base URL, e.g. https://api.github.com
	AppID string
	Key   *rsa.PrivateKey
	Org   string
	HTTP  *http.Client

	mu       sync.Mutex
	instID   int64
	token    string
	tokenExp time.Time
	etags    map[string]cached
	limit    limiter
}

type cached struct {
	etag string
	body []byte
}

// ParseKey reads an App private key in PEM (PKCS#1 as GitHub issues it, or PKCS#8).
func ParseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	b, _ := pem.Decode(pemBytes)
	if b == nil {
		return nil, errors.New("no PEM block in key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(b.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("key is not RSA")
	}
	return rk, nil
}

// jwt signs the short-lived RS256 token GitHub requires for App-level calls.
func (a *App) jwt() (string, error) {
	enc := base64.RawURLEncoding.EncodeToString
	now := time.Now().Unix()
	head := enc([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body := enc(fmt.Appendf(nil, `{"iat":%d,"exp":%d,"iss":%q}`, now-60, now+540, a.AppID))
	sum := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(nil, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return head + "." + body + "." + enc(sig), nil
}

// GitToken returns a cached installation token, refreshed when under 5 minutes remain.
func (a *App) GitToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.tokenExp) > 5*time.Minute {
		return a.token, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	if a.instID == 0 {
		var inst struct {
			ID int64 `json:"id"`
		}
		if err := a.call(ctx, "GET", a.API+"/orgs/"+a.Org+"/installation", jwt, &inst); err != nil {
			return "", fmt.Errorf("find installation: %w", err)
		}
		a.instID = inst.ID
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.call(ctx, "POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", a.API, a.instID), jwt, &tok); err != nil {
		return "", fmt.Errorf("installation token: %w", err)
	}
	a.token, a.tokenExp = tok.Token, tok.ExpiresAt
	return a.token, nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func (a *App) Repos(ctx context.Context) ([]Repo, error) {
	var all []Repo
	for url := a.API + "/installation/repositories?per_page=100"; url != ""; {
		var page struct {
			Repositories []Repo `json:"repositories"`
		}
		next, err := a.get(ctx, url, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Repositories...)
		url = next
	}
	return all, nil
}

func (a *App) Probe(ctx context.Context) error {
	_, err := a.get(ctx, a.API+"/rate_limit", nil) // rate_limit calls don't count against the limit
	return err
}

// get does an installation-authenticated GET with ETag reuse, decodes into v and returns the next page's URL.
func (a *App) get(ctx context.Context, url string, v any) (string, error) {
	tok, err := a.GitToken(ctx)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	c := a.etags[url]
	a.mu.Unlock()
	resp, body, err := a.do(ctx, "GET", url, tok, c.etag)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusUnauthorized {
			a.mu.Lock()
			a.token = "" // revoked or expired early: mint a new one next time
			a.mu.Unlock()
		}
		return "", err
	}
	if resp.StatusCode == http.StatusNotModified {
		body = c.body
	} else if etag := resp.Header.Get("ETag"); etag != "" {
		a.mu.Lock()
		if a.etags == nil {
			a.etags = map[string]cached{}
		}
		a.etags[url] = cached{etag, body}
		a.mu.Unlock()
	}
	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			return "", fmt.Errorf("decode %s: %w", url, err)
		}
	}
	next := ""
	if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		next = m[1]
	}
	return next, nil
}

// call does a request with an explicit bearer token and decodes the JSON response into v.
func (a *App) call(ctx context.Context, method, url, bearer string, v any) error {
	_, body, err := a.do(ctx, method, url, bearer, "")
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// do sends one request through the rate limiter. Non-2xx responses other than 304 become a *StatusError.
func (a *App) do(ctx context.Context, method, url, bearer, etag string) (*http.Response, []byte, error) {
	if err := a.limit.wait(ctx); err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	a.limit.update(resp, body)
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotModified {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &msg) // best effort: fall back to the raw body below
		if msg.Message == "" {
			msg.Message = string(bytes.TrimSpace(body))
		}
		return nil, nil, &StatusError{resp.StatusCode, msg.Message}
	}
	return resp, body, nil
}

// limiter follows GitHub's rate-limit headers: it waits for the reset when nothing remains,
// and on a block (403/429 with Retry-After or nothing remaining) stops all requests until it ends.
type limiter struct {
	mu    sync.Mutex
	until time.Time
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	d := time.Until(l.until)
	l.mu.Unlock()
	if d <= 0 {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d {
		return fmt.Errorf("%w: blocked for %s", ErrRateLimited, d.Round(time.Second))
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *limiter) update(resp *http.Response, body []byte) {
	h := resp.Header
	var until time.Time
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		until = time.Now().Add(time.Duration(s) * time.Second)
	} else if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			until = time.Unix(reset, 0)
		}
	}
	if until.IsZero() && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		bytes.Contains(bytes.ToLower(body), []byte("rate limit")) {
		// A secondary limit without Retry-After: GitHub says wait at least a minute.
		until = time.Now().Add(time.Minute)
	}
	if until.IsZero() {
		return
	}
	l.mu.Lock()
	if until.After(l.until) {
		l.until = until
	}
	l.mu.Unlock()
}

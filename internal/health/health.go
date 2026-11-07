// Package health probes GitHub's API and Git hosting separately.
package health

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/store"
)

type Result struct {
	OK        bool      `json:"ok"`
	CheckedAt time.Time `json:"checked_at"`
}

type Checker struct {
	GH     ghclient.Client
	HTTP   *http.Client
	GitURL string
	Store  *store.Store
	Log    *slog.Logger

	mu       sync.Mutex
	api, git Result
}

// Check runs both probes once.
func (c *Checker) Check(ctx context.Context) {
	apiErr := c.GH.Probe(ctx)
	gitErr := c.probeGit(ctx)
	now := time.Now().UTC()
	c.mu.Lock()
	c.api, c.git = Result{apiErr == nil, now}, Result{gitErr == nil, now}
	c.mu.Unlock()
	if apiErr != nil || gitErr != nil {
		c.Log.Warn("github unhealthy", "api_err", apiErr, "git_err", gitErr)
	}
}

// probeGit asks for a repo's ref advertisement without credentials. Any answer below 500, including
// the 401 a private repo gives, means Git hosting is up; it needs no API call for a token.
func (c *Checker) probeGit(ctx context.Context) error {
	url := c.GitURL + "/"
	if repos, err := c.Store.Repos(ctx); err == nil && len(repos) > 0 {
		url = fmt.Sprintf("%s/%s.git/info/refs?service=git-upload-pack", c.GitURL, repos[0].FullName())
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("git probe: %s", resp.Status)
	}
	return nil
}

func (c *Checker) Status() (api, git Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.api, c.git
}

// Run checks every interval until ctx ends, giving each check at most timeout.
func (c *Checker) Run(ctx context.Context, every, timeout time.Duration) {
	for {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		c.Check(cctx)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Package health probes GitHub on separate paths and alerts the admin when paths stop answering
// or GitHub turns slow or flaky (design 8.14). It never changes the mode: the admin does.
package health

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/store"
)

// Result is one probe's state, as /status shows it.
type Result struct {
	OK        bool      `json:"ok"` // the last check answered, in time
	CheckedAt time.Time `json:"checked_at"`
	ErrorRate float64   `json:"error_rate"` // failed or slow answers over the window
	DelayMS   int64     `json:"delay_ms"`   // the last check's delay
}

// Thresholds decide what the alerts call down, slow, flaky and healthy again (decision D6). Zero
// fields take DefaultThresholds'.
type Thresholds struct {
	HealthyFor time.Duration // every check answering in time for this long ends a flaky spell
	Window     time.Duration // the error rate is over this long
	AlertAfter time.Duration // a path not answering for this long is alerted
	SlowAfter  time.Duration // an answer slower than this counts as an error
	ErrorRate  float64       // this share of the window's checks failing or slow is a flaky spell
}

// DefaultThresholds are guesses, not yet set by testing against GitHub.
var DefaultThresholds = Thresholds{HealthyFor: 10 * time.Minute, Window: 5 * time.Minute, AlertAfter: 2 * time.Minute,
	SlowAfter: 5 * time.Second, ErrorRate: 0.2}

func (a Thresholds) withDefaults() Thresholds {
	d := DefaultThresholds
	a.HealthyFor, a.Window = cmp.Or(a.HealthyFor, d.HealthyFor), cmp.Or(a.Window, d.Window)
	a.AlertAfter, a.SlowAfter = cmp.Or(a.AlertAfter, d.AlertAfter), cmp.Or(a.SlowAfter, d.SlowAfter)
	a.ErrorRate = cmp.Or(a.ErrorRate, d.ErrorRate)
	return a
}

type Checker struct {
	GH      ghclient.Client // the signed-in API probe
	HTTP    *http.Client    // the anonymous probes
	API     string          // GitHub's API, for the anonymous API probe; "" skips it
	GitURL  string
	SSHAddr string // GitHub's SSH server as host:port; "" skips the probe
	Store   *store.Store
	Log     *slog.Logger
	Alert   func(msg string) // nil: log only
	Limits  Thresholds
	Now     func() time.Time // tests only

	mu      sync.Mutex
	probes  map[string]*probe
	started time.Time
	alerted string // the paths not answering at the last alert
	flaky   bool   // in a flaky spell, as last alerted
}

type probe struct {
	last              Result
	samples           []sample
	answered, healthy time.Time // the last answer, and the last check that wasn't an answer in time
}

type sample struct {
	at  time.Time
	bad bool
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

// Check runs every probe once, at the same time, then alerts as needed.
func (c *Checker) Check(ctx context.Context) {
	runs := map[string]func(context.Context) error{"api": c.GH.Probe, "git-https": c.probeGit}
	if c.API != "" {
		// rate_limit calls don't count against the limit, which is 60 an hour without signing in.
		runs["api-anon"] = func(ctx context.Context) error { return c.answers(ctx, c.API+"/rate_limit") }
	}
	if c.SSHAddr != "" {
		runs["git-ssh"] = c.probeSSH
	}
	type outcome struct {
		err   error
		delay time.Duration
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	got := map[string]outcome{}
	for name, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now() // real time even under a test clock: delays are real
			err := run(ctx)
			mu.Lock()
			got[name] = outcome{err, time.Since(t0)}
			mu.Unlock()
		}()
	}
	wg.Wait()

	a, now := c.Limits.withDefaults(), c.now()
	c.mu.Lock()
	if c.probes == nil {
		c.probes, c.started = map[string]*probe{}, now
	}
	var failing []any
	for name, o := range got {
		p := c.probes[name]
		if p == nil {
			p = &probe{answered: c.started, healthy: c.started}
			c.probes[name] = p
		}
		bad := o.err != nil || o.delay > a.SlowAfter
		if o.err == nil {
			p.answered = now
		}
		if bad {
			p.healthy = now
			failing = append(failing, name, cmp.Or(fmt.Sprint(o.err), "slow: "+o.delay.Round(time.Millisecond).String()))
		}
		p.samples = append(slices.DeleteFunc(p.samples, func(s sample) bool { return now.Sub(s.at) >= a.Window }), sample{now, bad})
		nbad := 0
		for _, s := range p.samples {
			if s.bad {
				nbad++
			}
		}
		p.last = Result{OK: !bad, CheckedAt: now, ErrorRate: float64(nbad) / float64(len(p.samples)), DelayMS: o.delay.Milliseconds()}
	}
	c.mu.Unlock()
	if len(failing) > 0 {
		c.Log.Warn("github unhealthy", failing...)
	}
	c.decide(ctx, a, now)
}

// answers GETs url without credentials. Any answer below 500 means the path is up, including the
// 401 a private repo gives.
func (c *Checker) answers(ctx context.Context, url string) error {
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
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

// probeGit asks for a repo's ref advertisement without credentials, so it needs no API call for a token.
func (c *Checker) probeGit(ctx context.Context) error {
	url := c.GitURL + "/"
	if repos, err := c.Store.Repos(ctx); err == nil && len(repos) > 0 {
		url = fmt.Sprintf("%s/%s.git/info/refs?service=git-upload-pack", c.GitURL, repos[0].FullName())
	}
	return c.answers(ctx, url)
}

// probeSSH reads the SSH server's greeting, which needs no key.
func (c *Checker) probeSSH(ctx context.Context) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.SSHAddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(d); err != nil {
			return err
		}
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if !strings.HasPrefix(line, "SSH-") {
		return fmt.Errorf("no SSH greeting: %q %v", line, err)
	}
	return nil
}

// Status returns each probe's state by name: api, api-anon, git-https and git-ssh.
func (c *Checker) Status() map[string]Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]Result{}
	for name, p := range c.probes {
		out[name] = p.last
	}
	return out
}

// decide alerts when the set of paths not answering changes, and when a flaky spell starts or
// ends. The alerts hint at what the admin could do; switching is theirs.
func (c *Checker) decide(ctx context.Context, a Thresholds, now time.Time) {
	c.mu.Lock()
	var down []string
	healthy, nbad, n := true, 0, 0
	for name, p := range c.probes {
		healthy = healthy && now.Sub(p.healthy) >= a.HealthyFor
		if now.Sub(p.answered) >= a.AlertAfter {
			down = append(down, name)
			continue // a path that's down is alerted, not "flaky": a blocked SSH port mustn't hint at degraded mode
		}
		for _, s := range p.samples {
			n++
			if s.bad {
				nbad++
			}
		}
	}
	slices.Sort(down)
	_, checksSSH := c.probes["git-ssh"]
	everyPath := len(down) == len(c.probes)
	flaky := now.Sub(c.started) >= a.Window && n > 0 && float64(nbad) >= a.ErrorRate*float64(n)
	c.mu.Unlock()

	mode, err := c.Store.Mode(ctx)
	if err != nil {
		c.Log.Error("health: read mode", "err", err)
		return
	}

	if set := strings.Join(down, ", "); set != c.alerted {
		c.alerted = set
		c.alert(downMessage(mode, down, checksSSH, everyPath))
	}
	switch {
	case flaky && !c.flaky:
		c.flaky = true
		msg := fmt.Sprintf("GitHub is slow or flaky: %d of the last %d checks failed or took over %s (alerted at %.0f%% over %s).",
			nbad, n, a.SlowAfter, a.ErrorRate*100, a.Window)
		if mode == store.Normal {
			msg += " If developers are held up, start degraded mode with `cove degraded start`."
		}
		c.alert(msg)
	case c.flaky && healthy:
		c.flaky = false
		msg := fmt.Sprintf("GitHub has answered every check in time for %s.", a.HealthyFor)
		switch mode {
		case store.Degraded:
			msg += " End degraded mode with `cove degraded stop`."
		case store.Paused:
			msg += " Resume replay with `cove degraded start`, or end degraded mode with `cove degraded stop`."
		}
		c.alert(msg)
	}
}

func downMessage(mode store.Mode, down []string, checksSSH, everyPath bool) string {
	if len(down) == 0 {
		return "GitHub answers again on every path checked."
	}
	msg := "GitHub isn't answering on: " + strings.Join(down, ", ") + "."
	ssh, https := slices.Contains(down, "git-ssh"), slices.Contains(down, "git-https")
	switch {
	case ssh && !https:
		msg += " Git over HTTPS still works: developers can switch with `git config --global url.https://github.com/.insteadOf git@github.com:`." +
			" (If GitCove's server can't reach GitHub's SSH port at all, set COVE_GITHUB_SSH=off.)"
	case https && !ssh && checksSSH:
		msg += " Git over SSH still works: developers can switch with `git config --global url.git@github.com:.insteadOf https://github.com/`."
	}
	if everyPath && mode != store.Failover {
		msg += " Start failover with `cove failover start`."
	}
	return msg
}

func (c *Checker) alert(msg string) {
	if c.Alert != nil {
		c.Alert(msg)
	} else {
		c.Log.Warn("health: " + msg)
	}
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

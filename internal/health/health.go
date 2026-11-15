// Package health probes GitHub on separate paths, alerts the admin when paths stop answering,
// and, if the admin opted in, switches the instance's mode by itself (design 8.14).
package health

import (
	"bufio"
	"cmp"
	"context"
	"errors"
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

// Auto is the switching the admin opted in to and its thresholds (decision D6). Zero thresholds
// take DefaultAuto's.
type Auto struct {
	FailoverAfter time.Duration // start failover once every probe has failed this long; 0: never
	Degraded      bool          // enter degraded mode once ErrorRate of the window's checks fail or are slow

	MinRun     time.Duration // an automatic failover lasts at least this long
	Cooldown   time.Duration // after an automatic failover or degraded mode ends, no new one for this long
	HealthyFor time.Duration // every check answering in time for this long ends either
	Window     time.Duration // the error rate is over this long
	AlertAfter time.Duration // a path not answering for this long is alerted
	SlowAfter  time.Duration // an answer slower than this counts as an error
	ErrorRate  float64
}

// DefaultAuto has decision D6's default thresholds; both opt-ins are off. Only the ten minutes
// before failover comes from the design, and it's a guess; the rest are guesses too.
var DefaultAuto = Auto{MinRun: 30 * time.Minute, Cooldown: 30 * time.Minute, HealthyFor: 10 * time.Minute,
	Window: 5 * time.Minute, AlertAfter: 2 * time.Minute, SlowAfter: 5 * time.Second, ErrorRate: 0.2}

func (a Auto) withDefaults() Auto {
	d := DefaultAuto
	a.MinRun, a.Cooldown, a.HealthyFor = cmp.Or(a.MinRun, d.MinRun), cmp.Or(a.Cooldown, d.Cooldown), cmp.Or(a.HealthyFor, d.HealthyFor)
	a.Window, a.AlertAfter, a.SlowAfter = cmp.Or(a.Window, d.Window), cmp.Or(a.AlertAfter, d.AlertAfter), cmp.Or(a.SlowAfter, d.SlowAfter)
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
	Auto    Auto
	Now     func() time.Time // tests only

	mu      sync.Mutex
	probes  map[string]*probe
	started time.Time
	alerted string // the paths not answering at the last alert

	// The mode as the checks last saw it, since when, and whether they set it themselves.
	loaded bool
	seen   store.Mode
	since  time.Time
	auto   bool
	ended  time.Time // when the checks last ended an automatic failover or degraded mode
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

// Check runs every probe once, at the same time, then alerts and switches modes as needed.
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

	a, now := c.Auto.withDefaults(), c.now()
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

// decide alerts when the set of paths not answering changes, and switches modes as the admin
// opted in to. It never ends a failover an admin started.
func (c *Checker) decide(ctx context.Context, a Auto, now time.Time) {
	c.mu.Lock()
	var down []string
	allDown, healthy, nbad, n := true, true, 0, 0
	for name, p := range c.probes {
		allDown = allDown && now.Sub(p.answered) >= a.FailoverAfter
		healthy = healthy && now.Sub(p.healthy) >= a.HealthyFor
		if now.Sub(p.answered) >= a.AlertAfter {
			down = append(down, name)
			continue // a path that's down is alerted, not "flaky": a blocked SSH port mustn't mean degraded mode
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
	windowFull := now.Sub(c.started) >= a.Window
	c.mu.Unlock()

	mode, err := c.Store.Mode(ctx)
	if err != nil {
		c.Log.Error("health: read mode", "err", err)
		return
	}
	c.load(ctx, mode, now)

	if set := strings.Join(down, ", "); set != c.alerted {
		c.alerted = set
		c.alert(downMessage(a, mode, down, checksSSH, everyPath))
	}

	cooled := now.Sub(c.ended) >= a.Cooldown
	switch {
	case (mode == store.Normal || mode == store.Degraded) && a.FailoverAfter > 0 && allDown && cooled:
		c.switchTo(ctx, store.Failover, now, fmt.Sprintf("every check of GitHub has failed for %s, so failover started by itself. "+
			"Developers' agents switch them to GitCove. It ends by itself once GitHub has been healthy for %s, or with `cove failover end`.",
			a.FailoverAfter, a.HealthyFor))
	case mode == store.Normal && a.Degraded && windowFull && n > 0 && float64(nbad) >= a.ErrorRate*float64(n) && cooled:
		c.switchTo(ctx, store.Degraded, now, fmt.Sprintf("%d of the last %d checks of GitHub failed or were slow, so GitCove entered degraded mode. "+
			"Developers' agents send their work to GitCove, and replay posts it to GitHub in the background. "+
			"It ends once GitHub has been healthy for %s and nothing is left to replay.", nbad, n, a.HealthyFor))
	case mode == store.Degraded && healthy:
		c.switchTo(ctx, store.Normal, now, fmt.Sprintf("GitHub has been healthy for %s and everything is replayed, so degraded mode ended.", a.HealthyFor))
	case mode == store.Failover && c.auto && now.Sub(c.since) >= a.MinRun && healthy:
		c.switchTo(ctx, store.Reconciling, now, fmt.Sprintf("GitHub has been healthy for %s, so the automatic failover ended. "+
			"Developers' agents switch them back to GitHub. Check `cove replay plan`, then `cove replay approve`.", a.HealthyFor))
	}
}

func downMessage(a Auto, mode store.Mode, down []string, checksSSH, everyPath bool) string {
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
	if everyPath && mode == store.Normal {
		if a.FailoverAfter > 0 {
			msg += fmt.Sprintf(" Failover starts by itself if this lasts %s.", a.FailoverAfter)
		} else {
			msg += " Start failover with `cove failover start`."
		}
	}
	return msg
}

// load learns, on the first check after a start, whether the checks set the current mode
// themselves and when they last ended one; afterwards it notices changes others make.
func (c *Checker) load(ctx context.Context, mode store.Mode, now time.Time) {
	if c.loaded {
		if mode != c.seen {
			c.seen, c.since, c.auto = mode, now, false
		}
		return
	}
	c.loaded, c.seen, c.since = true, mode, now
	h, err := c.Store.ModeHistory(ctx, "auto")
	if err != nil {
		c.Log.Error("health: read mode history", "err", err)
		return
	}
	if h.LastBy == "auto" {
		c.auto, c.since = true, h.LastAt
	}
	c.ended = h.EndedAt
}

func (c *Checker) switchTo(ctx context.Context, to store.Mode, now time.Time, msg string) {
	if err := c.Store.SetMode(ctx, "auto", to); err != nil {
		if !errors.Is(err, store.ErrWrongMode) { // e.g. degraded mode with items still to replay
			c.Log.Error("health: switch mode", "to", to, "err", err)
		}
		return
	}
	c.seen, c.since, c.auto = to, now, true
	if to == store.Reconciling || to == store.Normal {
		c.ended = now
	}
	c.alert(msg)
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

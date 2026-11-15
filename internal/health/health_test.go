package health

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/store"
)

// rig checks fakehub every 30 seconds of a fake clock.
type rig struct {
	t      *testing.T
	hub    *fakehub.Hub
	st     *store.Store
	c      *Checker
	clock  time.Time
	alerts []string
	repo   int64
}

func newRig(t *testing.T, lim Thresholds) *rig {
	hub := fakehub.New(t, 1)
	hub.AddRepo(t, "app")
	st, err := store.Open(filepath.Join(t.TempDir(), "cove.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := &rig{t: t, hub: hub, st: st, clock: time.Now().UTC()}
	if r.repo, err = st.UpsertRepo(context.Background(), fakehub.RepoID("app"), hub.Org, "app", "main"); err != nil {
		t.Fatal(err)
	}
	gh := &ghclient.App{API: r.hub.URL, AppID: r.hub.AppID, Key: r.hub.Key, Org: r.hub.Org, HTTP: &http.Client{Timeout: 2 * time.Second}}
	r.c = &Checker{GH: gh, HTTP: &http.Client{Timeout: 2 * time.Second}, API: r.hub.URL, GitURL: r.hub.URL, SSHAddr: r.hub.SSH,
		Store: r.st, Log: slog.New(slog.DiscardHandler), Limits: lim, Now: func() time.Time { return r.clock },
		Alert: func(msg string) { r.alerts = append(r.alerts, msg) }}
	return r
}

// run checks n times under faults f, alternating with healthy checks if flap is set, and fails
// if the mode ever changes: only the admin switches.
func (r *rig) run(n int, f fakehub.Faults, flap bool) {
	r.t.Helper()
	m := r.mode()
	for i := range n {
		if flap && i%2 == 1 {
			r.hub.SetFaults(fakehub.Faults{})
		} else {
			r.hub.SetFaults(f)
		}
		r.clock = r.clock.Add(30 * time.Second)
		r.c.Check(context.Background())
		if got := r.mode(); got != m {
			r.t.Fatalf("check %d of %d under %+v: mode %s, was %s", i+1, n, f, got, m)
		}
	}
}

func (r *rig) mode() store.Mode {
	m, err := r.st.Mode(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

func (r *rig) setMode(m store.Mode) {
	r.t.Helper()
	if err := r.st.SetMode(context.Background(), "cli", m); err != nil {
		r.t.Fatal(err)
	}
}

// lastAlert returns the latest alert and forgets them all.
func (r *rig) lastAlert() string {
	if len(r.alerts) == 0 {
		return ""
	}
	a := r.alerts[len(r.alerts)-1]
	r.alerts = nil
	return a
}

var down = fakehub.Faults{APIDown: true, GitDown: true, SSHDown: true}

// TestAlerts: the alerts name the broken paths, hint at failover or degraded mode, and never
// change the mode, even through a full outage.
func TestAlerts(t *testing.T) {
	r := newRig(t, Thresholds{SlowAfter: 100 * time.Millisecond})
	r.run(4, fakehub.Faults{}, false)
	if a := r.lastAlert(); a != "" || len(r.c.Status()) != 4 {
		t.Fatalf("healthy GitHub: alert %q, status %+v", a, r.c.Status())
	}

	// Flapping, slow and erroring for 40 minutes: one hint at degraded mode, no outage alert.
	r.run(80, down, true)
	r.run(30, fakehub.Faults{Delay: 200 * time.Millisecond}, false)
	r.run(30, fakehub.Faults{ErrorRate: 0.4}, false)
	if len(r.alerts) != 1 || !strings.Contains(r.alerts[0], "slow or flaky") || !strings.Contains(r.alerts[0], "20% over 5m0s") ||
		!strings.Contains(r.alerts[0], "cove degraded start") {
		t.Fatalf("alerts for a flaky GitHub that kept answering: %q", r.alerts)
	}
	r.alerts = nil
	r.run(20, fakehub.Faults{}, false) // ten healthy minutes end the spell
	if a := r.lastAlert(); a != "GitHub has answered every check in time for 10m0s." {
		t.Fatalf("healthy again: %q", a)
	}

	// The App can't get a token, but GitHub answers anyone else: GitCove's problem, not an outage.
	r.c.GH = &ghclient.App{API: r.hub.URL, AppID: r.hub.AppID, Key: r.hub.Key, Org: r.hub.Org, HTTP: &http.Client{Timeout: 2 * time.Second}}
	r.run(30, fakehub.Faults{TokenDown: true}, false)
	if a := r.lastAlert(); a != "GitHub isn't answering on: api." {
		t.Fatalf("token failure alert: %q", a)
	}

	r.run(6, fakehub.Faults{SSHDown: true}, false)
	if a := r.lastAlert(); !strings.Contains(a, "on: git-ssh.") || !strings.Contains(a, "insteadOf git@github.com:") {
		t.Fatalf("SSH-only alert: %q", a)
	}
	r.run(6, fakehub.Faults{GitDown: true}, false)
	if a := r.lastAlert(); !strings.Contains(a, "on: git-https.") || !strings.Contains(a, "insteadOf https://github.com/") {
		t.Fatalf("HTTPS-only alert: %q", a)
	}
	r.run(2, fakehub.Faults{}, false)
	if a := r.lastAlert(); a != "GitHub answers again on every path checked." {
		t.Fatalf("recovery alert: %q", a)
	}

	// A full outage hints at failover, for as long as it lasts, and starts nothing.
	r.run(60, down, false)
	if a := r.lastAlert(); !strings.Contains(a, "api, api-anon, git-https, git-ssh.") || !strings.Contains(a, "cove failover start") {
		t.Fatalf("outage alert: %q", a)
	}
}

// TestDegradedHints: in degraded mode the checks never end it, but say when GitHub looks healthy
// again (offering to resume replay if it's paused), and a full outage there hints at failover
// without starting it.
func TestDegradedHints(t *testing.T) {
	r := newRig(t, Thresholds{SlowAfter: 100 * time.Millisecond})
	r.run(20, fakehub.Faults{Delay: 200 * time.Millisecond}, false)
	if a := r.lastAlert(); !strings.Contains(a, "cove degraded start") {
		t.Fatalf("flaky alert: %q", a)
	}
	r.setMode(store.Degraded)
	r.run(60, fakehub.Faults{}, false)
	if a := r.lastAlert(); !strings.Contains(a, "cove degraded stop") || strings.Contains(a, "cove degraded start") {
		t.Fatalf("healthy alert in degraded mode: %q", a)
	}
	// Paused, the same alert also offers resuming.
	r.run(20, fakehub.Faults{Delay: 200 * time.Millisecond}, false)
	r.setMode(store.Paused)
	r.run(60, fakehub.Faults{}, false)
	if a := r.lastAlert(); !strings.Contains(a, "Resume replay with `cove degraded start`") || !strings.Contains(a, "cove degraded stop") {
		t.Fatalf("healthy alert while paused: %q", a)
	}
	r.setMode(store.Degraded)
	r.run(40, down, false)
	if a := r.lastAlert(); !strings.Contains(a, "cove failover start") {
		t.Fatalf("outage alert in degraded mode: %q", a)
	}
}

// TestBlockedSSH: a server that can't reach GitHub's SSH port at all gets an alert, not a hint at degraded mode.
func TestBlockedSSH(t *testing.T) {
	r := newRig(t, Thresholds{})
	r.run(40, fakehub.Faults{SSHDown: true}, false)
	if len(r.alerts) != 1 || !strings.Contains(r.alerts[0], "on: git-ssh.") || !strings.Contains(r.alerts[0], "COVE_GITHUB_SSH=off") {
		t.Fatalf("alerts: %q", r.alerts)
	}
}

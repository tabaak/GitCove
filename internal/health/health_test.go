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

func newRig(t *testing.T, auto Auto) *rig {
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
	r.c = r.checker(auto)
	return r
}

// checker is a fresh Checker on the rig's store, as after a restart.
func (r *rig) checker(auto Auto) *Checker {
	gh := &ghclient.App{API: r.hub.URL, AppID: r.hub.AppID, Key: r.hub.Key, Org: r.hub.Org, HTTP: &http.Client{Timeout: 2 * time.Second}}
	return &Checker{GH: gh, HTTP: &http.Client{Timeout: 2 * time.Second}, API: r.hub.URL, GitURL: r.hub.URL, SSHAddr: r.hub.SSH,
		Store: r.st, Log: slog.New(slog.DiscardHandler), Auto: auto, Now: func() time.Time { return r.clock },
		Alert: func(msg string) { r.alerts = append(r.alerts, msg) }}
}

// run checks n times under faults f, alternating with healthy checks if flap is set, and fails
// if the mode is ever one of never.
func (r *rig) run(n int, f fakehub.Faults, flap bool, never ...store.Mode) {
	r.t.Helper()
	for i := range n {
		if flap && i%2 == 1 {
			r.hub.SetFaults(fakehub.Faults{})
		} else {
			r.hub.SetFaults(f)
		}
		r.clock = r.clock.Add(30 * time.Second)
		r.c.Check(context.Background())
		for _, m := range never {
			if r.mode() == m {
				r.t.Fatalf("check %d of %d under %+v: mode %s", i+1, n, f, m)
			}
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

func (r *rig) want(m store.Mode) {
	r.t.Helper()
	if got := r.mode(); got != m {
		r.t.Fatalf("mode %s, want %s; alerts:\n%s", got, m, strings.Join(r.alerts, "\n"))
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

// TestNoFalseAlarms: with automatic failover on, nothing short of every path failing for ten
// minutes starts it, and the alerts name the broken paths.
func TestNoFalseAlarms(t *testing.T) {
	r := newRig(t, Auto{FailoverAfter: 10 * time.Minute, SlowAfter: 100 * time.Millisecond})
	r.run(4, fakehub.Faults{}, false)
	if a := r.lastAlert(); a != "" || len(r.c.Status()) != 4 {
		t.Fatalf("healthy GitHub: alert %q, status %+v", a, r.c.Status())
	}

	r.run(80, down, true, store.Failover) // flapping for 40 minutes
	r.run(30, fakehub.Faults{Delay: 200 * time.Millisecond}, false, store.Failover, store.Degraded)
	r.run(30, fakehub.Faults{ErrorRate: 0.4}, false, store.Failover, store.Degraded)
	r.want(store.Normal)
	if a := r.lastAlert(); a != "" {
		t.Fatalf("alert for a flaky GitHub that kept answering: %q", a)
	}

	// The App can't get a token, but GitHub answers anyone else: GitCove's problem, not an outage.
	r.c.GH = &ghclient.App{API: r.hub.URL, AppID: r.hub.AppID, Key: r.hub.Key, Org: r.hub.Org, HTTP: &http.Client{Timeout: 2 * time.Second}}
	r.run(30, fakehub.Faults{TokenDown: true}, false, store.Failover)
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
	r.want(store.Normal)
}

// TestAutoFailover: a full outage starts failover after ten minutes; it ends by itself no sooner
// than its minimum run and after GitHub has been healthy, then a cooldown holds off the next.
// An admin's failover is never ended by the checks, and a restart keeps track of an automatic one.
func TestAutoFailover(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, Auto{FailoverAfter: 10 * time.Minute})
	r.run(4, fakehub.Faults{}, false)
	r.run(19, down, false, store.Failover) // 9.5 minutes
	if a := r.lastAlert(); !strings.Contains(a, "api, api-anon, git-https, git-ssh.") || !strings.Contains(a, "starts by itself if this lasts 10m0s") {
		t.Fatalf("outage alert: %q", a)
	}
	r.run(1, down, false)
	r.want(store.Failover)
	if a := r.lastAlert(); !strings.Contains(a, "failover started by itself") {
		t.Fatalf("failover alert: %q", a)
	}
	r.run(59, fakehub.Faults{}, false, store.Reconciling) // healthy, but only 29.5 minutes since it started
	r.run(1, fakehub.Faults{}, false)
	r.want(store.Reconciling)
	if a := r.lastAlert(); !strings.Contains(a, "automatic failover ended") {
		t.Fatalf("end alert: %q", a)
	}
	for _, m := range []store.Mode{store.Replaying, store.Normal} { // the admin approves, replay finishes
		if err := r.st.SetMode(ctx, "cli", m); err != nil {
			t.Fatal(err)
		}
	}

	r.run(40, down, false, store.Failover) // 20 minutes, inside the cooldown
	r.run(20, down, false)
	r.want(store.Failover)

	// After a restart the new checker knows the failover was automatic, and ends it.
	r.c = r.checker(Auto{FailoverAfter: 10 * time.Minute})
	r.run(21, fakehub.Faults{}, false)
	r.want(store.Reconciling)

	// An admin's failover stays until the admin ends it.
	for _, m := range []store.Mode{store.Failover} {
		if err := r.st.SetMode(ctx, "cli", m); err != nil {
			t.Fatal(err)
		}
	}
	r.run(100, fakehub.Faults{}, false)
	r.want(store.Failover)
}

// TestDegraded: a slow or flaky GitHub puts GitCove in degraded mode, never failover, and it
// leaves once GitHub has been healthy for ten minutes and nothing is left to replay.
func TestDegraded(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, Auto{FailoverAfter: 10 * time.Minute, Degraded: true, SlowAfter: 100 * time.Millisecond})
	r.run(20, fakehub.Faults{ErrorRate: 0.01}, false, store.Degraded)
	r.run(30, fakehub.Faults{Delay: 200 * time.Millisecond}, false, store.Failover)
	r.want(store.Degraded)
	if a := r.lastAlert(); !strings.Contains(a, "entered degraded mode") {
		t.Fatalf("degraded alert: %q", a)
	}

	// Work arrives; GitHub recovers, but degraded mode lasts until the work is replayed.
	if err := r.st.RecordPush(ctx, r.repo, "ben", []store.RefUpdate{{Branch: "fix", Old: "", New: "abc"}}); err != nil {
		t.Fatal(err)
	}
	r.run(30, fakehub.Faults{}, false)
	r.want(store.Degraded)
	items, _ := r.st.RefItems(ctx, 0, "pending")
	if err := r.st.Finish(ctx, []int64{items[0].ID}, "done", "test", "", ""); err != nil {
		t.Fatal(err)
	}
	r.run(1, fakehub.Faults{}, false)
	r.want(store.Normal)

	// Flapping after the cooldown: degraded again, still never failover.
	r.run(60, fakehub.Faults{}, false)
	r.run(40, down, true, store.Failover)
	r.want(store.Degraded)

	// A real outage while degraded escalates to failover.
	r.run(21, down, false)
	r.want(store.Failover)
}

// TestBlockedSSH: a server that can't reach GitHub's SSH port at all gets an alert, not degraded mode.
func TestBlockedSSH(t *testing.T) {
	r := newRig(t, Auto{Degraded: true})
	r.run(40, fakehub.Faults{SSHDown: true}, false, store.Degraded)
	if a := r.lastAlert(); !strings.Contains(a, "on: git-ssh.") || !strings.Contains(a, "COVE_GITHUB_SSH=off") {
		t.Fatalf("alert: %q", a)
	}
}

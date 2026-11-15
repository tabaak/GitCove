package replay

import (
	"context"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/store"
)

// TestDegradedCampaign runs the push and PR campaigns in degraded mode (milestone 5's exit):
// GitHub answers slowly or flakily instead of being down, and replay runs in the background, with
// kills, while people push, open PRs, review, reply and merge. It checks what those campaigns
// check: all six of plan section 3. COVE_SEEDS sets the number of seeds (default 10; nightly 200).
// Repeat one with: COVE_SEEDS=N go test -run 'TestDegradedCampaign/pulls/seed=K$' ./internal/replay
func TestDegradedCampaign(t *testing.T) {
	seeds := 10
	if s, err := strconv.Atoi(os.Getenv("COVE_SEEDS")); err == nil {
		seeds = s
	}
	for name, campaign := range map[string]func(*testing.T, uint64, bool){"pushes": replayCampaign, "pulls": pullCampaign} {
		t.Run(name, func(t *testing.T) {
			for seed := uint64(1); seed <= uint64(seeds); seed++ {
				t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
					t.Parallel()
					campaign(t, seed, true)
				})
			}
		})
	}
}

// degraded puts the instance in degraded mode under flaky faults drawn from rng, and replays in
// the background until the returned stop is called.
func (e *env) degraded(t *testing.T, rng *mrand.Rand) (stop func()) {
	if m, _ := e.st.Mode(context.Background()); m != store.Degraded {
		e.mode(t, store.Degraded)
	}
	e.r.Grace = 5 * time.Second // under -race and parallel seeds, 300ms between the hook and the ref moving was too short
	e.gh.HTTP.Timeout, e.m.FetchTimeout = 500*time.Millisecond, 3*time.Second
	e.hub.SetFaults(fakehub.Faults{
		ErrorRate:     rng.Float64() * 0.4,
		TimeoutRate:   rng.Float64() * 0.1,
		CutRate:       rng.Float64() * 0.2,
		LostRate:      rng.Float64() * 0.4,
		RateLimitRate: rng.Float64() * 0.1,
		ListingLag:    time.Duration(rng.Float64() * float64(300*time.Millisecond)),
		Delay:         time.Duration(rng.Float64() * float64(50*time.Millisecond)),
	})
	kills := mrand.New(mrand.NewPCG(rng.Uint64(), 1)) // its own: the background can't share rng
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			r := *e.r // a fresh Replayer, as after a restart
			r.Kill = func(string) error {
				if kills.Float64() < 0.15 {
					return errKilled
				}
				return nil
			}
			_ = r.Pass(context.Background())
		}
	}()
	return func() { close(done); wg.Wait() }
}

// TestUnlandedPushWaits stops a push between the hook recording it and git moving the branch,
// where degraded mode's background replay can see it. Replay must leave it, or it would settle
// it as already on GitHub, the branch would follow GitHub again, and the push would be lost.
func TestUnlandedPushWaits(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 1, "app")
	e.r.Grace = 500 * time.Millisecond
	e.mode(t, store.Degraded)
	dev := e.clone(t, "app")
	sha := work(t, dev, "feature", "f.txt") // committed, not pushed: GitCove doesn't have it yet
	tip := e.served(t, "app", "refs/heads/feature")
	if err := e.st.RecordPush(ctx, e.repoID(t), "ben", []store.RefUpdate{{Branch: "feature", Old: tip, New: sha}}); err != nil {
		t.Fatal(err)
	}
	if err := e.r.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.items(t)["feature"][0]; it.State != "pending" {
		t.Fatalf("an unlanded push was settled: %s, %s", it.State, it.Result)
	}
	if held, _ := e.st.HeldBranches(ctx, e.repoID(t)); !held["feature"] {
		t.Fatal("the branch isn't held while its push is landing")
	}
	time.Sleep(e.r.Grace) // it never lands: the push failed, and replay settles it
	if err := e.r.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.items(t)["feature"][0]; it.State != "done" || it.Result != string(Already) {
		t.Fatalf("a failed push after the grace period: %s, %s", it.State, it.Result)
	}
}

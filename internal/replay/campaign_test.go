package replay

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/store"
)

// TestReplayCampaign runs seeded failovers: developers push while GitHub is down and GitHub moves
// on, then replay runs with faults, kills at every crash point, lost responses and repeat outages.
// Afterwards it checks R1 to R4 and that a full sync agrees (plan section 3, checks 1, 2, 3 and 6).
// COVE_SEEDS sets the number of seeds (default 10; the nightly run uses 200).
// Repeat one with: COVE_SEEDS=N go test -run 'TestReplayCampaign/seed=K$' ./internal/replay
func TestReplayCampaign(t *testing.T) {
	seeds := 10
	if s, err := strconv.Atoi(os.Getenv("COVE_SEEDS")); err == nil {
		seeds = s
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			replayCampaign(t, seed, false)
		})
	}
}

var errKilled = errors.New("killed")

// replayCampaign runs failovers, or with degraded set, rounds of degraded mode: GitHub flaky
// instead of down, and replay running while developers push (see TestDegradedCampaign).
func replayCampaign(t *testing.T, seed uint64, degraded bool) {
	ctx := context.Background()
	e := setup(t, seed, "app")
	rng := mrand.New(mrand.NewPCG(seed, 7))
	hubDir, coveDir := e.hub.Dir("app"), e.r.Git.Dir(fakehub.RepoID("app"))
	protectMain := rng.IntN(2) == 0
	if protectMain {
		e.hub.Protect(t, "app", "main")
	}
	dev := e.clone(t, "app")
	branches := []string{"main", "feature", "f1", "f2", "f3"}
	accepted := map[string]string{} // commit -> branch, for every push GitCove accepted
	type copiedTip struct {
		sha     string
		prs     int     // PRs that existed when developers pushed sha to GitHub themselves
		pending []int64 // items for the branch then waiting to replay
	}
	copied := map[string]copiedTip{} // branch -> what developers pushed to GitHub, while still the branch's tip on GitCove
	quick := func() { e.gh.HTTP.Timeout, e.m.FetchTimeout = 500*time.Millisecond, 3*time.Second }
	slow := func() { e.gh.HTTP.Timeout, e.m.FetchTimeout = 5*time.Second, 30*time.Second }

	refused := 0
	outage := func() {
		e.hub.SetFaults(fakehub.Faults{})
		slow()
		if err := e.m.Cycle(ctx); err != nil { // the regular sync, before GitHub fails
			t.Fatal(err)
		}
		stop := func() {}
		if degraded {
			stop = e.degraded(t, rng)
		} else {
			e.hub.SetFaults(fakehub.Faults{APIDown: true, GitDown: true})
			e.mode(t, store.Failover)
		}
		for range 2 + rng.IntN(5) {
			b := branches[rng.IntN(len(branches))]
			file := fmt.Sprintf("%s-%d.txt", b, rng.IntN(1e6))
			if rng.IntN(8) == 0 {
				file = ".github/workflows/ci.yml"
			}
			sha := work(t, dev, b, file)
			if out, err := push(dev, b); err == nil {
				accepted[sha] = b
				delete(copied, b)
			} else if degraded && !strings.Contains(out, "GitCove:") {
				refused++ // a sync moved the branch on GitCove meanwhile; the developer pulls and pushes again
			} else if !(protectMain && b == "main" && strings.Contains(out, "protected")) {
				t.Fatalf("push to %s refused: %s", b, out)
			}
			switch rng.IntN(14) { // meanwhile, on GitHub (rarely main, so its work often could fast-forward)
			case 0:
				e.hub.Commit(t, "app", "main")
			case 1:
				e.hub.Commit(t, "app", "feature")
			case 2:
				e.hub.CommitFile(t, "app", "main", ".github/workflows/ci.yml", fmt.Sprint(rng.IntN(1e6)))
			case 3: // a developer pushes a GitCove branch to GitHub themselves, if GitHub would take it
				if degraded {
					break // replay may push it meanwhile, so the checks below for this case can't tell who was first
				}
				b := branches[rng.IntN(len(branches))]
				if b == "main" && protectMain {
					break
				}
				if _, err := git(hubDir, "fetch", "-q", coveDir, "refs/heads/"+b); err != nil {
					break
				}
				sha := mustGit(t, hubDir, "rev-parse", "FETCH_HEAD")
				old, _ := git(hubDir, "rev-parse", "-q", "--verify", "refs/heads/"+b)
				if _, err := git(hubDir, "merge-base", "--is-ancestor", old, sha); old == "" || err == nil {
					mustGit(t, hubDir, "update-ref", "refs/heads/"+b, sha)
					c := copiedTip{sha: sha, prs: len(e.hub.PRs("app"))}
					for _, it := range e.items(t)[b] {
						if it.State == "pending" || it.State == "sending" {
							c.pending = append(c.pending, it.ID)
						}
					}
					copied[b] = c
				}
			}
		}
		stop()
		if !degraded {
			e.hub.SetFaults(fakehub.Faults{})
			e.mode(t, store.Reconciling, store.Replaying)
		}
	}

	outage()
	outages, passes, kills := 1, 0, 0
	for ; passes < 40; passes++ {
		if m, _ := e.st.Mode(ctx); m == store.Normal {
			break
		}
		quick()
		e.hub.SetFaults(fakehub.Faults{
			APIDown:     rng.Float64() < 0.1,
			GitDown:     rng.Float64() < 0.1,
			ErrorRate:   rng.Float64() * 0.2,
			TimeoutRate: rng.Float64() * 0.1,
			CutRate:     rng.Float64() * 0.2,
			LostRate:    rng.Float64() * 0.4,
		})
		// A fresh Replayer each pass, as after a restart: everything it knows is in the database.
		r := *e.r
		r.Kill = func(point string) error {
			if point == "sending" && rng.IntN(5) == 0 { // GitHub moves between replay's fetch and its push
				e.hub.Commit(t, "app", branches[1+rng.IntN(len(branches)-1)])
			}
			if rng.Float64() < 0.15 {
				kills++
				return errKilled
			}
			return nil
		}
		_ = r.Pass(ctx) // errors expected
		if outages < 3 && rng.IntN(3) == 0 {
			outage() // GitHub fails again mid-replay; developers push to the same branches
			outages++
		}
	}
	e.hub.SetFaults(fakehub.Faults{})
	slow()
	time.Sleep(e.r.Grace) // pushes recorded but never landed are settled after it
	for i := 0; i < 5; i++ {
		if m, _ := e.st.Mode(ctx); m == store.Normal {
			break
		}
		if err := e.r.Pass(ctx); err != nil {
			t.Fatalf("clean pass: %v", err)
		}
	}
	if degraded {
		e.mode(t, store.Normal) // what `cove degraded stop` does once nothing is left
	}
	if m, _ := e.st.Mode(ctx); m != store.Normal {
		t.Fatalf("mode %s after replay", m)
	}
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d outages, %d faulty passes, %d kills, %d pushes accepted, %d refused, %d PRs", outages, passes, kills, len(accepted), refused, len(e.hub.PRs("app")))

	// 1. Nothing lost (R2): every item is final, and every pushed commit is on GitHub or kept in a conflict.
	held := map[string]bool{}
	prOf := map[string]string{} // fallback branch -> item result
	for b, items := range e.items(t) {
		for _, it := range items {
			switch it.State {
			case "done":
				if strings.HasPrefix(it.Result, "PR #") {
					prOf[it.Target] = it.Result
				}
			case "conflict":
				held[b] = true
			default:
				t.Fatalf("item %d (%s) ended %s", it.ID, b, it.State)
			}
		}
	}
	for sha, b := range accepted {
		if onGitHub(e.hub, "app", sha) {
			continue
		}
		tip := e.served(t, "app", "refs/heads/"+b)
		if ok, _ := e.r.Git.IsAncestor(ctx, fakehub.RepoID("app"), sha, tip); !held[b] || !ok {
			t.Errorf("commit %s pushed to %s is neither on GitHub nor kept in a conflict", sha, b)
		}
	}

	// Commits developers had already pushed to GitHub are recognised, not sent again.
	for b, c := range copied {
		for _, it := range e.items(t)[b] {
			if slices.Contains(c.pending, it.ID) && it.Result != string(Already) {
				t.Errorf("%s was on GitHub before replay, but item %d ended %s: %s", b, it.ID, it.State, it.Result)
			}
		}
	}

	// 2. Nothing duplicated (R3): one PR per fallback branch, and it's the one replay recorded.
	prs := e.hub.PRs("app")
	heads := map[string]int{}
	for i, pr := range prs {
		heads[pr.Head]++
		if heads[pr.Head] > 1 {
			t.Errorf("second PR from %s", pr.Head)
		}
		if want := fmt.Sprintf("PR #%d from %s", pr.Number, pr.Head); prOf[pr.Head] != want {
			t.Errorf("PR #%d from %s, but replay recorded %q", pr.Number, pr.Head, prOf[pr.Head])
		}
		// Commits developers had already pushed to GitHub get no fallback PR.
		if c, ok := copied[pr.Base]; ok && i >= c.prs && e.hub.Refs(t, "app")["refs/heads/"+pr.Head] == c.sha {
			t.Errorf("PR #%d for commits already on GitHub", pr.Number)
		}
	}

	// 3. GitHub refs moved only by fast-forward (R1), never deleted, and main only through a PR (R4).
	for _, p := range e.hub.Pushes("app") {
		if p.Ref == "refs/heads/main" || strings.Trim(p.New, "0") == "" {
			t.Errorf("replay pushed %s %s", p.Ref, p.New)
		}
		if strings.Trim(p.Old, "0") != "" {
			if _, err := git(hubDir, "merge-base", "--is-ancestor", p.Old, p.New); err != nil {
				t.Errorf("non-fast-forward push to %s: %s -> %s", p.Ref, p.Old, p.New)
			}
		}
	}

	// 6. A full sync agrees with GitHub, except branches held for a human.
	ghRefs := e.hub.Refs(t, "app")
	servedRefs, err := e.r.Git.Refs(ctx, coveDir)
	if err != nil {
		t.Fatal(err)
	}
	for ref, sha := range servedRefs {
		if strings.HasPrefix(ref, "refs/gitcove/") || held[strings.TrimPrefix(ref, "refs/heads/")] {
			continue
		}
		if ghRefs[ref] != sha {
			t.Errorf("served %s = %s, GitHub has %q", ref, sha, ghRefs[ref])
		}
	}
	for ref := range ghRefs {
		if _, ok := servedRefs[ref]; !ok && !held[strings.TrimPrefix(ref, "refs/heads/")] {
			t.Errorf("GitHub's %s isn't served", ref)
		}
	}
}

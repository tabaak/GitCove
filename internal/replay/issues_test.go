package replay

// Milestone 3 against fakehub: changes to issues made during failover, and their replay.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/auth"
	"github.com/tabaak/gitcove/internal/store"
)

func (e *env) repoID(t *testing.T) int64 {
	t.Helper()
	r, err := e.st.RepoByName(context.Background(), e.hub.Org, "app")
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// rec records a change by the holder of tokenID (1 is ben, from setup).
func (e *env) rec(t *testing.T, tokenID int64, kind, issue string, c store.Change) string {
	t.Helper()
	if c.Author == "" {
		c.Author = map[int64]string{1: "ben"}[tokenID]
	}
	l, err := e.st.Record(context.Background(), e.repoID(t), tokenID, kind, issue, c)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (e *env) metaItems(t *testing.T) []store.Item {
	t.Helper()
	items, err := e.st.MetaItems(context.Background(), 0, "pending", "sending", "done", "skipped", "conflict", "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// replayAll ends the failover and replays until the instance is back to normal.
func (e *env) replayAll(t *testing.T) {
	t.Helper()
	e.mode(t, store.Reconciling, store.Replaying)
	for range 5 {
		if err := e.r.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if m, _ := e.st.Mode(context.Background()); m == store.Normal {
			return
		}
	}
	t.Fatalf("still replaying; items: %+v", e.metaItems(t))
}

func commentsOn(hub *fakehub.Hub, n int) []fakehub.Comment {
	var out []fakehub.Comment
	for _, c := range hub.Comments("app") {
		if c.Number == n {
			out = append(out, c)
		}
	}
	return out
}

func TestIssueReplay(t *testing.T) {
	e := setup(t, 1, "app")
	e.hub.AddIssue("app", "carol", "it breaks", "steps")
	if err := e.m.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.mode(t, store.Failover)
	e.hub.SetFaults(fakehub.Faults{APIDown: true, GitDown: true})
	l := e.rec(t, 1, "issue", "", store.Change{Title: "fix it", Body: "for #1", Labels: []string{"bug"}})
	e.rec(t, 1, "comment", l, store.Change{Body: "working on #" + l})
	e.rec(t, 1, "comment", "1", store.Change{Body: "see #" + l})
	e.rec(t, 1, "label", "1", store.Change{Label: "enhancement", Add: true})
	e.rec(t, 1, "state", "1", store.Change{State: "closed"})
	e.rec(t, 1, "edit", "1", store.Change{Field: "title", Value: "it breaks (#" + l + ")"})
	e.hub.SetFaults(fakehub.Faults{})
	e.replayAll(t)

	issues := e.hub.Issues("app")
	if len(issues) != 2 {
		t.Fatalf("GitHub has %d issues, want 2", len(issues))
	}
	one, two := issues[0], issues[1]
	if two.Title != "fix it" || two.Author != fakehub.Bot || !strings.HasPrefix(two.Body, "Originally by `ben` at ") ||
		!strings.Contains(two.Body, "for #1") || !strings.Contains(two.Body, "<!-- gitcove:op=") || strings.Join(two.Labels, ",") != "bug" {
		t.Errorf("new issue: %+v", two)
	}
	if one.State != "closed" || one.Title != "it breaks (#2)" || strings.Join(one.Labels, ",") != "enhancement" {
		t.Errorf("#1 after replay: %+v", one)
	}
	if c := commentsOn(e.hub, 2); len(c) != 1 || !strings.Contains(c[0].Body, "working on #2") {
		t.Errorf("comments on #2: %+v", c)
	}
	if c := commentsOn(e.hub, 1); len(c) != 1 || !strings.Contains(c[0].Body, "see #2") {
		t.Errorf("comments on #1: %+v", c)
	}
	for _, it := range e.metaItems(t) {
		if it.State != "done" {
			t.Errorf("item %d %s ended %s: %s", it.ID, it.Kind, it.State, it.Result)
		}
	}
	// A sync afterwards shows GitHub's version on GitCove.
	if err := e.m.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := e.st.Issue(context.Background(), e.repoID(t), 2); err != nil || got.Title != "fix it" {
		t.Errorf("mirror #2: %+v %v", got, err)
	}
}

// Design 8.6: GitHub's version stays when both sides changed the same text, and the proposal is
// posted as a note. A change GitHub already made is done; one GitHub didn't touch is applied.
func TestIssueConflicts(t *testing.T) {
	e := setup(t, 1, "app")
	e.hub.AddIssue("app", "carol", "title", "body")
	if err := e.m.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.mode(t, store.Failover)
	e.rec(t, 1, "edit", "1", store.Change{Field: "title", Value: "ben's title"})
	e.rec(t, 1, "edit", "1", store.Change{Field: "title", Value: "ben's second title"}) // based on the first
	e.rec(t, 1, "edit", "1", store.Change{Field: "body", Value: "ben's body"})
	e.rec(t, 1, "state", "1", store.Change{State: "closed"})
	e.rec(t, 1, "label", "1", store.Change{Label: "bug", Add: false}) // not on the issue
	e.hub.SetIssue("app", 1, "title", "GitHub's title")
	e.hub.SetIssue("app", 1, "state", "closed")
	e.replayAll(t)

	one := e.hub.Issues("app")[0]
	if one.Title != "GitHub's title" || one.Body != "ben's body" || one.State != "closed" {
		t.Errorf("#1 after replay: %+v", one)
	}
	notes := commentsOn(e.hub, 1)
	if len(notes) != 2 || !strings.Contains(notes[0].Body, "> ben's title") || !strings.Contains(notes[1].Body, "> ben's second title") {
		t.Errorf("notes: %+v", notes)
	}
	for _, it := range e.metaItems(t) {
		if it.State != "done" {
			t.Errorf("item %d ended %s: %s", it.ID, it.State, it.Result)
		}
	}
}

// Writes whose answers are lost are found by their markers, not posted again (cases 3 and 4).
func TestLostResponsesFound(t *testing.T) {
	e := setup(t, 1, "app")
	e.hub.AddIssue("app", "carol", "title", "body")
	e.mode(t, store.Failover)
	l := e.rec(t, 1, "issue", "", store.Change{Title: "new", Body: "b"})
	e.rec(t, 1, "comment", l, store.Change{Body: "c"})
	e.rec(t, 1, "comment", "1", store.Change{Body: "d"})
	e.hub.SetFaults(fakehub.Faults{LostRate: 1})
	e.replayAll(t)
	if n := len(e.hub.Issues("app")); n != 2 {
		t.Errorf("GitHub has %d issues, want 2", n)
	}
	if n := len(e.hub.Comments("app")); n != 2 {
		t.Errorf("GitHub has %d comments, want 2", n)
	}
	for _, it := range e.metaItems(t) {
		if it.State != "done" {
			t.Errorf("item %d ended %s: %s", it.ID, it.State, it.Result)
		}
	}
}

// Case 6: GitHub created the issue but its listing lags longer than the wait. The item ends
// uncertain, never reposted, and what depends on it waits for a human.
func TestCase6Uncertain(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 1, "app")
	e.mode(t, store.Failover)
	l := e.rec(t, 1, "issue", "", store.Change{Title: "new", Body: "b"})
	e.rec(t, 1, "comment", l, store.Change{Body: "c"})
	e.hub.SetFaults(fakehub.Faults{LostRate: 1, ListingLag: time.Hour})
	e.mode(t, store.Reconciling, store.Replaying)
	for range 3 {
		if err := e.r.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.hub.Issues("app")); n != 1 {
		t.Fatalf("GitHub has %d issues, want exactly 1", n)
	}
	items := e.metaItems(t)
	if items[0].State != "uncertain" || items[1].State != "pending" {
		t.Fatalf("items: %+v", items)
	}
	plan, err := e.r.IssuePlan(ctx)
	if err != nil || len(plan) != 1 || plan[0].Waits != "waits for L-1, which is uncertain" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if m, _ := e.st.Mode(ctx); m != store.Replaying {
		t.Fatalf("mode %s with an item waiting", m)
	}
	for _, it := range items {
		if err := e.st.Resolve(ctx, "test", it.ID, false); err != nil && it.State == "uncertain" {
			t.Fatal(err)
		}
	}
}

// A rate-limit refusal leaves the item pending, not uncertain, and replay resumes after it.
func TestRateLimitRequeues(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 1, "app")
	e.hub.AddIssue("app", "carol", "title", "body")
	e.mode(t, store.Failover)
	e.rec(t, 1, "comment", "1", store.Change{Body: "c"})
	e.hub.SetFaults(fakehub.Faults{RateLimitRate: 1})
	e.mode(t, store.Reconciling, store.Replaying)
	if err := e.r.Pass(ctx); err == nil {
		t.Fatal("pass succeeded under a rate limit")
	}
	if it := e.metaItems(t)[0]; it.State != "pending" {
		t.Fatalf("item %s after a rate limit, want pending", it.State)
	}
	e.hub.SetFaults(fakehub.Faults{})
	if err := e.r.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(e.hub.Comments("app")); n != 1 {
		t.Fatalf("GitHub has %d comments, want 1", n)
	}
}

// Decision D3: work by someone who authorised GitCove is posted as them, with refresh tokens
// rotated and saved; others' work, and theirs once the token is revoked, is posted by the App (R7).
func TestUserTokens(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 1, "app")
	e.hub.AddIssue("app", "carol", "title", "body")
	e.hub.AccessTTL = time.Minute // under the 5-minute margin, so every use refreshes
	alice, _, err := e.st.CreateToken(ctx, "test", "alice", []string{e.hub.Org + "/app"})
	if err != nil {
		t.Fatal(err)
	}
	users, err := auth.New(e.gh, e.st, slog.New(slog.DiscardHandler), []byte(strings.Repeat("ab", 32)))
	if err != nil {
		t.Fatal(err)
	}
	e.r.Users = users
	login, err := users.Authorise(ctx, alice, "test", func(code, uri string) {
		if !e.hub.ApproveDevice(code, "alice-gh") {
			t.Errorf("unknown code %q", code)
		}
	})
	if err != nil || login != "alice-gh" {
		t.Fatalf("authorise: %q %v", login, err)
	}

	e.mode(t, store.Failover)
	e.rec(t, alice, "comment", "1", store.Change{Author: "alice", Body: "from alice"})
	e.rec(t, alice, "issue", "", store.Change{Author: "alice", Title: "alice's", Body: "b"})
	e.rec(t, 1, "comment", "1", store.Change{Body: "from ben"})
	e.replayAll(t)
	c := commentsOn(e.hub, 1)
	if len(c) != 2 || c[0].Author != "alice-gh" || strings.Contains(c[0].Body, "Originally by") || !strings.Contains(c[0].Body, "Written on GitCove at") {
		t.Errorf("alice's comment: %+v", c)
	}
	if len(c) == 2 && (c[1].Author != fakehub.Bot || !strings.HasPrefix(c[1].Body, "Originally by `ben`")) {
		t.Errorf("ben's comment: %+v", c[1])
	}
	// Both of alice's items refreshed her token: the second worked only if the first saved the rotated one.
	if is := e.hub.Issues("app"); len(is) != 2 || is[1].Author != "alice-gh" {
		t.Errorf("alice's issue: %+v", is)
	}

	e.hub.RevokeUser("alice-gh")
	e.mode(t, store.Failover)
	e.rec(t, alice, "comment", "1", store.Change{Author: "alice", Body: "after revoking"})
	e.replayAll(t)
	if c := commentsOn(e.hub, 1); len(c) != 3 || c[2].Author != fakehub.Bot || !strings.HasPrefix(c[2].Body, "Originally by `alice`") {
		t.Errorf("after revoking: %+v", c)
	}
	toks, _ := e.st.Tokens(ctx)
	for _, tk := range toks {
		if tk.GitHub != "" {
			t.Errorf("token %s still linked to %s", tk.Name, tk.GitHub)
		}
	}
}

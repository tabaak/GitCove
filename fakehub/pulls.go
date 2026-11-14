package fakehub

// PRs, reviews and line comments. GitHub's rules that replay depends on are enforced: a review's
// commit must be part of the PR (spike I), an approval or change request must be for the PR's
// current head and not by its author (spike U), and a reply needs its parent to still exist.

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type Review struct {
	ID                  int64
	Number              int
	Author, State, Body string
	Commit              string
	Submitted           time.Time
	visible             time.Time
}

type ReviewComment struct {
	ID, ReviewID, InReplyTo int64
	Number                  int
	Author, Path            string
	Line                    int
	Side, Commit, Body      string
	Created                 time.Time
	visible                 time.Time
}

func (h *Hub) sha(name, rev string) string {
	out, _ := exec.Command(h.git, "-C", h.Dir(name), "rev-parse", "-q", "--verify", rev+"^{commit}").Output()
	return strings.TrimSpace(string(out))
}

func (h *Hub) isAncestor(name, a, b string) bool {
	return a != "" && b != "" && exec.Command(h.git, "-C", h.Dir(name), "merge-base", "--is-ancestor", a, b).Run() == nil
}

// prJSON renders a copy of a PR; the head commit is read from the repo, as the branch may have moved.
func (h *Hub) prJSON(name string, i *Issue) map[string]any {
	full := h.Org + "/" + name
	headRepo := full
	if i.Fork {
		headRepo = "someone/" + name
	}
	var merged any
	if i.Merged {
		merged = stamp(i.Updated)
	}
	return map[string]any{"number": i.Number, "state": i.State, "title": i.Title, "body": i.Body, "merged": i.Merged, "merged_at": merged,
		"user":       map[string]any{"login": i.Author},
		"head":       map[string]any{"ref": i.Head, "sha": h.sha(name, "refs/heads/"+i.Head), "repo": map[string]any{"full_name": headRepo}},
		"base":       map[string]any{"ref": i.Base, "repo": map[string]any{"full_name": full}},
		"created_at": stamp(i.Created), "updated_at": stamp(i.Updated)}
}

func (h *Hub) getPR(w http.ResponseWriter, r *http.Request) {
	var pr *Issue
	h.issue(w, r, func(_ *meta, i *Issue) (int, any) {
		if !i.PR {
			return http.StatusNotFound, map[string]any{"message": "Not Found"}
		}
		c := *i
		pr = &c
		return http.StatusOK, nil
	}, func() any { return h.prJSON(r.PathValue("repo"), pr) })
}

func reviewJSON(v *Review) map[string]any {
	return map[string]any{"id": v.ID, "user": map[string]any{"login": v.Author}, "body": v.Body, "state": v.State,
		"commit_id": v.Commit, "submitted_at": stamp(v.Submitted)}
}

func (h *Hub) reviewCommentJSON(org, repo string, c *ReviewComment) map[string]any {
	m := map[string]any{"id": c.ID, "pull_request_review_id": c.ReviewID, "user": map[string]any{"login": c.Author},
		"path": c.Path, "line": c.Line, "original_line": c.Line, "side": c.Side, "commit_id": c.Commit, "original_commit_id": c.Commit, "body": c.Body,
		"pull_request_url": h.URL + "/repos/" + org + "/" + repo + "/pulls/" + strconv.Itoa(c.Number),
		"created_at":       stamp(c.Created), "updated_at": stamp(c.Created)}
	if c.InReplyTo != 0 {
		m["in_reply_to_id"] = c.InReplyTo
	}
	return m
}

func (h *Hub) listReviews(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.PathValue("n"))
	h.mu.Lock()
	var list []*Review
	for _, v := range h.meta(r.PathValue("repo")).reviews {
		if v.Number == n && !time.Now().Before(v.visible) {
			c := *v
			list = append(list, &c)
		}
	}
	h.mu.Unlock()
	h.listing(w, r, len(list), func(k int) time.Time { return list[k].Submitted }, func(k int) time.Time { return list[k].Submitted },
		func(k int) map[string]any { return reviewJSON(list[k]) })
}

// listReviewComments serves the repo-wide, per-PR and per-review listings.
func (h *Hub) listReviewComments(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.PathValue("n"))
	review, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	h.mu.Lock()
	var list []*ReviewComment
	for _, c := range h.meta(r.PathValue("repo")).reviewComments {
		if (n == 0 || c.Number == n) && (review == 0 || c.ReviewID == review) && !time.Now().Before(c.visible) {
			cc := *c
			list = append(list, &cc)
		}
	}
	h.mu.Unlock()
	h.listing(w, r, len(list), func(k int) time.Time { return list[k].Created }, func(k int) time.Time { return list[k].Created },
		func(k int) map[string]any {
			return h.reviewCommentJSON(r.PathValue("org"), r.PathValue("repo"), list[k])
		})
}

func (h *Hub) createReview(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	var in struct {
		CommitID string `json:"commit_id"`
		Event    string
		Body     string
		Comments []struct {
			Path string
			Line int
			Side string
			Body string
		}
	}
	states := map[string]string{"COMMENT": "COMMENTED", "APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED"}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || states[in.Event] == "" || in.Body == "" && len(in.Comments) == 0 {
		h.json(w, r, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
		return
	}
	author := h.actor(r)
	h.issue(w, r, func(m *meta, i *Issue) (int, any) {
		if !i.PR {
			return http.StatusNotFound, map[string]any{"message": "Not Found"}
		}
		head := h.sha(name, "refs/heads/"+i.Head)
		switch {
		case !h.isAncestor(name, in.CommitID, head) || h.isAncestor(name, in.CommitID, i.BaseSHA):
			return http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed", "errors": []map[string]any{{
				"message": "commit_id is not part of the pull request", "resource": "PullRequestReview", "field": "commit_id", "code": "invalid"}}}
		case in.Event != "COMMENT" && in.CommitID != head:
			return http.StatusUnprocessableEntity, map[string]any{"message": "Unprocessable Entity"}
		case in.Event != "COMMENT" && author == i.Author:
			return invalid("Can not approve your own pull request")
		}
		t := now()
		h.nextComment++
		v := &Review{ID: h.nextComment, Number: i.Number, Author: author, State: states[in.Event], Body: in.Body, Commit: in.CommitID, Submitted: t, visible: h.lag()}
		m.reviews = append(m.reviews, v)
		for _, c := range in.Comments {
			h.nextComment++
			m.reviewComments = append(m.reviewComments, &ReviewComment{ID: h.nextComment, ReviewID: v.ID, Number: i.Number, Author: author,
				Path: c.Path, Line: c.Line, Side: c.Side, Commit: in.CommitID, Body: c.Body, Created: t, visible: v.visible})
		}
		i.Updated = t
		return http.StatusOK, reviewJSON(v)
	})
}

func (h *Hub) createReply(w http.ResponseWriter, r *http.Request) {
	var in struct{ Body string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Body == "" {
		h.json(w, r, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
		return
	}
	parent, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	author := h.actor(r)
	h.issue(w, r, func(m *meta, i *Issue) (int, any) {
		k := slices.IndexFunc(m.reviewComments, func(c *ReviewComment) bool { return c.ID == parent && c.Number == i.Number })
		if k < 0 {
			return http.StatusNotFound, map[string]any{"message": "Not Found"}
		}
		p := m.reviewComments[k]
		root := p.ID
		if p.InReplyTo != 0 {
			root = p.InReplyTo
		}
		h.nextComment++
		t := now()
		c := &ReviewComment{ID: h.nextComment, InReplyTo: root, Number: i.Number, Author: author, Path: p.Path, Line: p.Line, Side: p.Side,
			Commit: p.Commit, Body: in.Body, Created: t, visible: h.lag()}
		m.reviewComments = append(m.reviewComments, c)
		i.Updated = t
		return http.StatusCreated, h.reviewCommentJSON(r.PathValue("org"), r.PathValue("repo"), c)
	})
}

// AddPR opens a PR from head into base as author on GitHub's side and returns its number.
func (h *Hub) AddPR(name, author, head, base, title, body string) int {
	baseSHA := h.sha(name, "refs/heads/"+base)
	h.mu.Lock()
	defer h.mu.Unlock()
	m, t := h.meta(name), now()
	m.issues = append(m.issues, &Issue{Number: len(m.issues) + 1, PR: true, Head: head, Base: base, BaseSHA: baseSHA, Title: title, Body: body,
		FirstBody: body, State: "open", Author: author, Created: t, Updated: t})
	return len(m.issues)
}

// MergePR merges PR n on GitHub's side with a merge commit, as a person pressing the button would.
func (h *Hub) MergePR(t testing.TB, name string, n int) {
	h.mu.Lock()
	pr := *h.meta(name).issues[n-1]
	h.mu.Unlock()
	dir := h.Dir(name)
	base := h.gitT(t, dir, "rev-parse", "refs/heads/"+pr.Base)
	tree, _, _ := strings.Cut(h.gitT(t, dir, "merge-tree", "--write-tree", base, "refs/heads/"+pr.Head), "\n")
	merge := h.gitT(t, dir, "commit-tree", tree, "-p", base, "-p", h.sha(name, "refs/heads/"+pr.Head), "-m", "Merge pull request #"+strconv.Itoa(n))
	h.gitT(t, dir, "update-ref", "refs/heads/"+pr.Base, merge, base)
	h.mu.Lock()
	defer h.mu.Unlock()
	i := h.meta(name).issues[n-1]
	i.State, i.Merged, i.Updated = "closed", true, now()
}

// AddReviewComment leaves a line comment on PR n as author on GitHub's side and returns its ID.
func (h *Hub) AddReviewComment(name string, n int, author, path string, line int, body string) int64 {
	commit := ""
	h.mu.Lock()
	head := h.meta(name).issues[n-1].Head
	h.mu.Unlock()
	commit = h.sha(name, "refs/heads/"+head)
	h.mu.Lock()
	defer h.mu.Unlock()
	m, t := h.meta(name), now()
	h.nextComment++
	m.reviewComments = append(m.reviewComments, &ReviewComment{ID: h.nextComment, Number: n, Author: author, Path: path, Line: line, Side: "RIGHT",
		Commit: commit, Body: body, Created: t})
	m.issues[n-1].Updated = t
	return h.nextComment
}

// DeleteReviewComment deletes a line comment on GitHub's side.
func (h *Hub) DeleteReviewComment(name string, id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.meta(name)
	m.reviewComments = slices.DeleteFunc(m.reviewComments, func(c *ReviewComment) bool { return c.ID == id })
}

// Reviews returns copies of repo name's reviews, oldest first.
func (h *Hub) Reviews(name string) []Review {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Review
	for _, v := range h.meta(name).reviews {
		out = append(out, *v)
	}
	return out
}

// ReviewComments returns copies of repo name's line comments, oldest first.
func (h *Hub) ReviewComments(name string) []ReviewComment {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []ReviewComment
	for _, c := range h.meta(name).reviewComments {
		out = append(out, *c)
	}
	return out
}

package fakehub

// Issues, PRs, comments and labels; the App's identity; and user tokens through the device flow.

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Bot is the login of fakehub's App, the author of what the App posts.
const Bot = "cove-fake[bot]"

// Issue is an issue or PR. Issues and PRs share numbers, as on GitHub.
type Issue struct {
	Number           int
	Title, Body      string
	FirstBody        string // the body it was created with
	State            string
	Author           string
	Labels           []string
	PR               bool
	Head, Base       string // PRs only
	BaseSHA          string // PRs only: the base's commit when the PR was opened
	Merged, Fork     bool   // PRs only
	Created, Updated time.Time
	visible          time.Time // listings show it from then on: the listing-lag fault
}

type Comment struct {
	ID               int64
	Number           int
	Author, Body     string
	Created, Updated time.Time
	visible          time.Time
}

type meta struct {
	issues   []*Issue
	comments []*Comment
	labels   []string

	reviews        []*Review
	reviewComments []*ReviewComment
}

type device struct{ userCode, login string }

// meta returns repo name's issue data, creating it. Callers hold h.mu.
func (h *Hub) meta(name string) *meta {
	m := h.metas[name]
	if m == nil {
		m = &meta{labels: []string{"bug", "documentation", "enhancement"}}
		h.metas[name] = m
	}
	return m
}

// now is GitHub's clock: one-second resolution.
func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

func stamp(t time.Time) string { return t.Format(time.RFC3339) }

// lag is when an item created now through the API appears in listings. Callers hold h.mu.
func (h *Hub) lag() time.Time {
	return time.Now().Add(time.Duration(h.rng.Float64() * float64(h.faults.ListingLag)))
}

func issueJSON(i *Issue) map[string]any {
	labels := []map[string]any{}
	for _, l := range i.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	m := map[string]any{"number": i.Number, "title": i.Title, "body": i.Body, "state": i.State,
		"user": map[string]any{"login": i.Author}, "labels": labels, "created_at": stamp(i.Created), "updated_at": stamp(i.Updated)}
	if i.PR {
		m["pull_request"] = map[string]any{"head": i.Head}
	}
	return m
}

func (h *Hub) commentJSON(org, repo string, c *Comment) map[string]any {
	return map[string]any{"id": c.ID, "body": c.Body, "user": map[string]any{"login": c.Author},
		"issue_url":  fmt.Sprintf("%s/repos/%s/%s/issues/%d", h.URL, org, repo, c.Number),
		"created_at": stamp(c.Created), "updated_at": stamp(c.Updated)}
}

// listing sorts, filters by since, and pages items like GitHub's list endpoints.
func (h *Hub) listing(w http.ResponseWriter, r *http.Request, n int, created, updated func(int) time.Time, render func(int) map[string]any) {
	q := r.URL.Query()
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	if s := q.Get("since"); s != "" {
		since, err := time.Parse(time.RFC3339, s)
		if err != nil {
			h.json(w, r, http.StatusUnprocessableEntity, map[string]any{"message": "Invalid since"})
			return
		}
		idx = slices.DeleteFunc(idx, func(i int) bool { return updated(i).Before(since) })
	}
	key := created
	if q.Get("sort") == "updated" {
		key = updated
	}
	slices.SortStableFunc(idx, func(a, b int) int { return key(a).Compare(key(b)) })
	if q.Get("direction") == "desc" || q.Get("direction") == "" && q.Get("sort") != "" {
		slices.Reverse(idx)
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	if perPage <= 0 || perPage > 100 {
		perPage = 30
	}
	page := max(1, func() int { p, _ := strconv.Atoi(q.Get("page")); return p }())
	out := []map[string]any{}
	for k, i := range idx {
		if k >= (page-1)*perPage && k < page*perPage {
			out = append(out, render(i))
		}
	}
	if page*perPage < len(idx) {
		u := *r.URL
		q.Set("page", strconv.Itoa(page+1))
		u.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, h.URL, u.RequestURI()))
	}
	h.json(w, r, http.StatusOK, out)
}

func (h *Hub) listIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state, creator := cmp.Or(q.Get("state"), "open"), q.Get("creator")
	h.mu.Lock()
	var list []*Issue
	for _, i := range h.meta(r.PathValue("repo")).issues {
		if !time.Now().Before(i.visible) && (state == "all" || i.State == state) && (creator == "" || i.Author == creator) {
			c := *i
			list = append(list, &c)
		}
	}
	h.mu.Unlock()
	h.listing(w, r, len(list), func(k int) time.Time { return list[k].Created }, func(k int) time.Time { return list[k].Updated },
		func(k int) map[string]any { return issueJSON(list[k]) })
}

func (h *Hub) listComments(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.PathValue("n")) // 0 for the repo-wide listing
	h.mu.Lock()
	var list []*Comment
	for _, c := range h.meta(r.PathValue("repo")).comments {
		if !time.Now().Before(c.visible) && (n == 0 || c.Number == n) {
			cc := *c
			list = append(list, &cc)
		}
	}
	h.mu.Unlock()
	h.listing(w, r, len(list), func(k int) time.Time { return list[k].Created }, func(k int) time.Time { return list[k].Updated },
		func(k int) map[string]any { return h.commentJSON(r.PathValue("org"), r.PathValue("repo"), list[k]) })
}

// issue finds issue n of the request's repo and calls f with h.mu held, or answers 404. A render
// function, if given, makes the body of a 200 after h.mu is released.
func (h *Hub) issue(w http.ResponseWriter, r *http.Request, f func(m *meta, i *Issue) (int, any), render ...func() any) {
	n, _ := strconv.Atoi(r.PathValue("n"))
	h.mu.Lock()
	m := h.meta(r.PathValue("repo"))
	status, out := http.StatusNotFound, any(map[string]any{"message": "Not Found"})
	if n >= 1 && n <= len(m.issues) {
		status, out = f(m, m.issues[n-1])
	}
	h.mu.Unlock()
	if status == http.StatusOK && len(render) > 0 {
		out = render[0]()
	}
	h.json(w, r, status, out)
}

func (h *Hub) getIssue(w http.ResponseWriter, r *http.Request) {
	h.issue(w, r, func(_ *meta, i *Issue) (int, any) { return http.StatusOK, issueJSON(i) })
}

func invalid(msg string) (int, any) {
	return http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed", "errors": []map[string]any{{"message": msg}}}
}

func (h *Hub) createIssue(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title, Body string
		Labels      []string
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Title == "" {
		h.json(w, r, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
		return
	}
	author := h.actor(r)
	h.mu.Lock()
	m := h.meta(r.PathValue("repo"))
	t := now()
	i := &Issue{Number: len(m.issues) + 1, Title: in.Title, Body: in.Body, FirstBody: in.Body, State: "open", Author: author,
		Created: t, Updated: t, visible: h.lag()}
	for _, l := range in.Labels {
		addLabel(m, i, l)
	}
	m.issues = append(m.issues, i)
	out := issueJSON(i)
	h.mu.Unlock()
	h.json(w, r, http.StatusCreated, out)
}

func (h *Hub) editIssue(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		h.json(w, r, http.StatusBadRequest, map[string]any{"message": "Problems parsing JSON"})
		return
	}
	h.issue(w, r, func(_ *meta, i *Issue) (int, any) {
		if s, ok := in["state"]; ok && s != "open" && s != "closed" {
			return invalid("state must be open or closed")
		}
		for f, v := range in {
			setField(i, f, v)
		}
		i.Updated = now()
		return http.StatusOK, issueJSON(i)
	})
}

func setField(i *Issue, field, value string) {
	switch field {
	case "title":
		i.Title = value
	case "body":
		i.Body = value
	case "state":
		i.State = value
	}
}

func (h *Hub) createComment(w http.ResponseWriter, r *http.Request) {
	var in struct{ Body string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Body == "" {
		h.json(w, r, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
		return
	}
	author := h.actor(r)
	h.issue(w, r, func(m *meta, i *Issue) (int, any) {
		h.nextComment++
		t := now()
		c := &Comment{ID: h.nextComment, Number: i.Number, Author: author, Body: in.Body, Created: t, Updated: t, visible: h.lag()}
		m.comments = append(m.comments, c)
		i.Updated = t
		return http.StatusCreated, h.commentJSON(r.PathValue("org"), r.PathValue("repo"), c)
	})
}

// addLabel puts label on i, creating it in the repo if new, as GitHub does.
func addLabel(m *meta, i *Issue, label string) {
	if !slices.Contains(m.labels, label) {
		m.labels = append(m.labels, label)
	}
	if !slices.Contains(i.Labels, label) {
		i.Labels = append(i.Labels, label)
	}
}

func (h *Hub) addLabels(w http.ResponseWriter, r *http.Request) {
	var in struct{ Labels []string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Labels) == 0 {
		h.json(w, r, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
		return
	}
	h.issue(w, r, func(m *meta, i *Issue) (int, any) {
		for _, l := range in.Labels {
			addLabel(m, i, l)
		}
		i.Updated = now()
		return http.StatusOK, issueJSON(i)["labels"]
	})
}

func (h *Hub) removeLabel(w http.ResponseWriter, r *http.Request) {
	h.issue(w, r, func(_ *meta, i *Issue) (int, any) {
		k := slices.Index(i.Labels, r.PathValue("label"))
		if k < 0 {
			return http.StatusNotFound, map[string]any{"message": "Label does not exist"}
		}
		i.Labels = slices.Delete(i.Labels, k, k+1)
		i.Updated = now()
		return http.StatusOK, issueJSON(i)["labels"]
	})
}

func (h *Hub) repoLabels(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	out := []map[string]any{}
	for _, l := range h.meta(r.PathValue("repo")).labels {
		out = append(out, map[string]any{"name": l})
	}
	h.mu.Unlock()
	h.json(w, r, http.StatusOK, out)
}

func (h *Hub) appInfo(w http.ResponseWriter, r *http.Request) {
	h.json(w, r, http.StatusOK, map[string]any{"id": 1, "slug": strings.TrimSuffix(Bot, "[bot]")})
}

// actor is who a request's token acts as: the App's bot, a user, or "" for no valid token.
func (h *Hub) actor(r *http.Request) string {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.validToken(tok) {
		return Bot
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if u, ok := h.userTokens[tok]; ok && time.Now().Before(u.exp) {
		return u.login
	}
	return ""
}

type userToken struct {
	login string
	exp   time.Time
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *Hub) deviceCode(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("client_id") != ClientID {
		h.json(w, r, http.StatusOK, map[string]any{"error": "unauthorized_client"})
		return
	}
	code, user := randHex(20), strings.ToUpper(randHex(2)+"-"+randHex(2))
	h.mu.Lock()
	h.devices[code] = &device{userCode: user}
	h.mu.Unlock()
	h.json(w, r, http.StatusOK, map[string]any{"device_code": code, "user_code": user,
		"verification_uri": h.URL + "/login/device", "expires_in": 900, "interval": 0})
}

// ApproveDevice is login entering userCode at /login/device.
func (h *Hub) ApproveDevice(userCode, login string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range h.devices {
		if d.userCode == userCode {
			d.login = login
			return true
		}
	}
	return false
}

func (h *Hub) oauthToken(w http.ResponseWriter, r *http.Request) {
	out := h.grant(r)
	h.json(w, r, http.StatusOK, out)
}

// grant answers a token request: a device code once approved, or a refresh token, which rotates (spike U).
func (h *Hub) grant(r *http.Request) map[string]any {
	fail := func(code string) map[string]any { return map[string]any{"error": code, "error_description": code} }
	if r.FormValue("client_id") != ClientID {
		return fail("incorrect_client_credentials")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var login string
	switch r.FormValue("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		d := h.devices[r.FormValue("device_code")]
		switch {
		case d == nil:
			return fail("expired_token")
		case d.login == "":
			return fail("authorization_pending")
		}
		login = d.login
		delete(h.devices, r.FormValue("device_code"))
	case "refresh_token":
		if r.FormValue("client_secret") != ClientSecret {
			return fail("incorrect_client_credentials")
		}
		var ok bool
		if login, ok = h.refresh[r.FormValue("refresh_token")]; !ok {
			return fail("bad_refresh_token")
		}
		delete(h.refresh, r.FormValue("refresh_token"))
	default:
		return fail("unsupported_grant_type")
	}
	access, refresh := "ghu_"+randHex(18), "ghr_"+randHex(18)
	ttl := cmp.Or(h.AccessTTL, 8*time.Hour)
	h.userTokens[access] = userToken{login, time.Now().Add(ttl)}
	h.refresh[refresh] = login
	return map[string]any{"access_token": access, "expires_in": int(ttl.Seconds()), "refresh_token": refresh,
		"refresh_token_expires_in": 15638400, "token_type": "bearer", "scope": ""}
}

func (h *Hub) user(w http.ResponseWriter, r *http.Request) {
	h.json(w, r, http.StatusOK, map[string]any{"login": h.actor(r)})
}

// RevokeUser revokes every token login has given the App, as removing its authorisation would.
func (h *Hub) RevokeUser(login string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for t, u := range h.userTokens {
		if u.login == login {
			delete(h.userTokens, t)
		}
	}
	for t, l := range h.refresh {
		if l == login {
			delete(h.refresh, t)
		}
	}
}

// Issues returns copies of repo name's issues and PRs, by number.
func (h *Hub) Issues(name string) []Issue {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Issue
	for _, i := range h.meta(name).issues {
		c := *i
		c.Labels = slices.Clone(i.Labels)
		out = append(out, c)
	}
	return out
}

// Comments returns copies of repo name's comments, oldest first.
func (h *Hub) Comments(name string) []Comment {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Comment
	for _, c := range h.meta(name).comments {
		out = append(out, *c)
	}
	return out
}

// AddIssue opens an issue as author on GitHub's side and returns its number.
func (h *Hub) AddIssue(name, author, title, body string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, t := h.meta(name), now()
	m.issues = append(m.issues, &Issue{Number: len(m.issues) + 1, Title: title, Body: body, FirstBody: body, State: "open", Author: author, Created: t, Updated: t})
	return len(m.issues)
}

// SetIssue changes field (title, body or state) of issue n on GitHub's side.
func (h *Hub) SetIssue(name string, n int, field, value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := h.meta(name).issues[n-1]
	setField(i, field, value)
	i.Updated = now()
}

// AddComment comments on issue n as author on GitHub's side.
func (h *Hub) AddComment(name string, n int, author, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, t := h.meta(name), now()
	h.nextComment++
	m.comments = append(m.comments, &Comment{ID: h.nextComment, Number: n, Author: author, Body: body, Created: t, Updated: t})
	m.issues[n-1].Updated = t
}

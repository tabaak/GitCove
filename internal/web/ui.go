package web

// The server-rendered interface (design 7 and 8): browse repos, issues, PRs and comments, and
// during failover comment, open issues, label, close, reopen and edit. Every change is queued in
// the outbox for replay; GitHub's copy is shown as last synced, with queued changes on top.

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tabaak/gitcove/internal/store"
)

//go:embed templates.html
var templateFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{"when": when, "dict": dict}).ParseFS(templateFS, "templates.html"))

// when shows an RFC 3339 timestamp as "2 Jan 2026, 15:04 UTC", or as given if it doesn't parse.
func when(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2 Jan 2006, 15:04 UTC")
}

// dict builds a map from pairs, to pass a sub-template more than one value.
func dict(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

const cookieName = "cove_token"

// GitHub allows 65536 characters in a body; this leaves room for the attribution line and marker.
const maxBody = 60000

// page is what every template gets.
type page struct {
	User     store.Token
	Repo     store.Repo
	Mode     store.Mode
	Writable bool     // in failover or degraded mode: changes are taken
	Age      string   // how old GitCove's copy of the repo is
	Stale    bool     // the copy may be out of date: old, or its last sync failed
	Down     []string // GitHub's paths that failed the last health check
	OnTheWay int      // changes still to replay to GitHub
	Data     any
}

// plainToken is the GitCove token from the session cookie or, for scripts, the Basic auth password.
func plainToken(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	_, tok, _ := r.BasicAuth()
	return tok
}

// sameOrigin rejects cross-site form posts. The cookie is SameSite=Strict too; this also covers Basic auth.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o == "" || o == "http://"+r.Host || o == "https://"+r.Host
}

// ui wraps a handler that needs a signed-in user, and a repo they may read if the path names one.
func (s *Server) ui(h func(http.ResponseWriter, *http.Request, *page) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		t, err := s.Store.TokenFor(ctx, plainToken(r))
		if errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		} else if err != nil {
			s.fail(w, err)
			return
		}
		if r.Method == "POST" {
			if !sameOrigin(r) {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		p := &page{User: t}
		if p.Mode, err = s.Store.Mode(ctx); err != nil {
			s.fail(w, err)
			return
		}
		p.Writable = p.Mode.TakesWrites()
		if p.OnTheWay, err = s.Store.Unreplayed(ctx); err != nil {
			s.fail(w, err)
			return
		}
		if s.Health != nil {
			for name, res := range s.Health.Status() {
				if !res.OK {
					p.Down = append(p.Down, name)
				}
			}
			slices.Sort(p.Down)
		}
		if owner := r.PathValue("owner"); owner != "" {
			full := owner + "/" + r.PathValue("repo")
			if p.Repo, err = s.Store.RepoByName(ctx, owner, r.PathValue("repo")); err != nil || !slices.Contains(t.Repos, full) {
				http.NotFound(w, r) // the same answer for a repo without access, so names can't be probed
				return
			}
			p.Age, p.Stale = s.age(p.Repo)
		}
		err = h(w, r, p)
		var bad badRequest
		switch {
		case errors.As(err, &bad):
			http.Error(w, string(bad), http.StatusBadRequest)
		case errors.Is(err, store.ErrWrongMode):
			http.Error(w, "GitCove takes changes only during a failover or degraded mode. Make them on GitHub.", http.StatusConflict)
		case errors.Is(err, store.ErrNotFound):
			http.NotFound(w, r)
		case err != nil:
			s.fail(w, err)
		}
	}
}

type badRequest string

func (b badRequest) Error() string { return string(b) }

func (s *Server) age(r store.Repo) (string, bool) {
	t, err := time.Parse(time.RFC3339Nano, r.SyncedAt)
	if err != nil {
		return "never synced", true
	}
	ago := time.Since(t)
	return "copy of GitHub from " + ago.Round(time.Second).String() + " ago", r.SyncError != "" || s.StaleAfter > 0 && ago > s.StaleAfter
}

func (s *Server) render(w http.ResponseWriter, name string, p *page) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, name, p)
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if err := s.render(w, "login", &page{}); err != nil {
		s.fail(w, err)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.FormValue("token"))
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	if _, err := s.Store.TokenFor(r.Context(), tok); err != nil {
		http.Error(w, "that isn't a valid GitCove token", http.StatusUnauthorized)
		return
	}
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", MaxAge: 30 * 24 * 3600,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, p *page) error {
	p.Data = p.User.Repos
	return s.render(w, "home", p)
}

// row is an issue in the list.
type row struct {
	Ref, Num            string // number or L-n for a new issue not on GitHub yet; Num as shown, e.g. #12
	Title, State, Badge string
	IsPR                bool
	Labels              []string
}

func (s *Server) issues(w http.ResponseWriter, r *http.Request, p *page) error {
	ctx := r.Context()
	want := r.URL.Query().Get("state")
	if want != "closed" && want != "all" {
		want = "open"
	}
	items, err := s.Store.MetaItems(ctx, p.Repo.ID, "pending", "sending", "conflict", "uncertain")
	if err != nil {
		return err
	}
	var rows []row
	for _, it := range items {
		if it.Kind == "issue" && want != "closed" {
			rows = append(rows, row{Ref: it.Issue, Num: it.Issue, Title: it.Change.Title, State: "open", Badge: status(it.State), Labels: it.Change.Labels})
		}
	}
	mirrored, err := s.Store.Issues(ctx, p.Repo.ID)
	if err != nil {
		return err
	}
	for _, i := range mirrored {
		if want == "all" || i.State == want {
			rows = append(rows, row{Ref: strconv.Itoa(i.Number), Num: "#" + strconv.Itoa(i.Number), Title: i.Title, State: i.State, IsPR: i.IsPR, Labels: i.Labels})
		}
	}
	p.Data = struct {
		State string
		Rows  []row
	}{want, rows}
	return s.render(w, "issues", p)
}

func status(state string) string {
	return map[string]string{"pending": "queued", "sending": "replaying", "conflict": "needs an admin", "uncertain": "needs an admin"}[state]
}

// post is a comment, from GitHub or queued.
type post struct{ Author, Body, At, Badge string }

// thread is an issue as GitHub last showed it, with queued changes applied on top.
type thread struct {
	Ref                       string
	Title, Body, State        string
	Author, At                string
	IsPR, Local, EditableBody bool
	Labels, AddLabels         []string
	Posts                     []post
	Queued                    []string
	PR                        *prView // nil for an issue
}

// thread builds the issue page for ref. It returns the GitHub number instead if ref is a new
// issue that is now on GitHub, so the caller can redirect.
func (s *Server) thread(r *http.Request, p *page, ref string) (thread, int, error) {
	ctx := r.Context()
	th := thread{Ref: ref, EditableBody: true}
	items, err := s.Store.MetaItems(ctx, p.Repo.ID, "pending", "sending", "conflict", "uncertain")
	if err != nil {
		return th, 0, err
	}
	sort.Slice(items, func(a, b int) bool { return items[a].ID < items[b].ID })
	if strings.HasPrefix(ref, "L-") {
		n, state, err := s.Store.MappedIssue(ctx, p.Repo.ID, ref)
		switch {
		case err != nil:
			return th, 0, err
		case n != 0:
			return th, n, nil
		case state == "" || state == "skipped":
			return th, 0, store.ErrNotFound
		}
		for _, it := range items {
			if it.Kind == "issue" && it.Issue == ref {
				c := it.Change
				th.Title, th.Body, th.State, th.Author, th.At, th.Labels = c.Title, c.Body, "open", c.Author, it.Created, slices.Clone(c.Labels)
				th.Local, th.EditableBody = true, it.State == "pending"
				what := "new issue, "
				if c.Head != "" {
					th.IsPR, th.PR, what = true, &prView{Head: c.Head, Base: c.Base}, "new pull request, "
				}
				th.Queued = append(th.Queued, what+status(it.State))
			}
		}
	} else {
		n, err := strconv.Atoi(ref)
		if err != nil {
			return th, 0, store.ErrNotFound
		}
		i, err := s.Store.Issue(ctx, p.Repo.ID, n)
		if err != nil {
			return th, 0, err
		}
		th.Title, th.Body, th.State, th.Author, th.At, th.IsPR, th.Labels = i.Title, i.Body, i.State, i.Author, i.CreatedAt, i.IsPR, i.Labels
		comments, err := s.Store.Comments(ctx, p.Repo.ID, n)
		if err != nil {
			return th, 0, err
		}
		for _, c := range comments {
			th.Posts = append(th.Posts, post{c.Author, c.Body, c.CreatedAt, ""})
		}
	}
	// Queued shows each change's net effect, not every click: the latest change per label, state or
	// field, and nothing once it is back to how it started.
	baseLabels, baseState, base := slices.Clone(th.Labels), th.State, map[string]string{"title": th.Title, "body": th.Body}
	net := map[string]string{}
	var order []string
	note := func(key, desc string, undone bool) {
		if undone {
			delete(net, key)
			return
		}
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
		net[key] = desc
	}
	for _, it := range items {
		if it.Issue != ref || it.Kind == "issue" {
			continue
		}
		c, badge := it.Change, status(it.State)
		switch it.Kind {
		case "comment":
			th.Posts = append(th.Posts, post{c.Author, c.Body, it.Created, badge})
		case "label":
			if c.Add && !slices.Contains(th.Labels, c.Label) {
				th.Labels = append(th.Labels, c.Label)
			} else if !c.Add {
				th.Labels = slices.DeleteFunc(th.Labels, func(l string) bool { return l == c.Label })
			}
			note("label "+c.Label, fmt.Sprintf("label %s %s by %s, %s", map[bool]string{true: "+", false: "-"}[c.Add], c.Label, c.Author, badge),
				slices.Contains(baseLabels, c.Label) == c.Add)
		case "state":
			th.State = c.State
			if c.Via != "" {
				note("state", fmt.Sprintf("closed by merging %s on GitCove", c.Via), false)
				continue
			}
			note("state", fmt.Sprintf("%s by %s, %s", map[string]string{"open": "reopened", "closed": "closed"}[c.State], c.Author, badge), c.State == baseState)
		case "edit":
			if c.Field == "title" {
				th.Title = c.Value
			} else {
				th.Body = c.Value
			}
			note("edit "+c.Field, fmt.Sprintf("%s edited by %s, %s", c.Field, c.Author, badge), c.Value == base[c.Field])
		}
	}
	for _, k := range order {
		if d, ok := net[k]; ok {
			th.Queued = append(th.Queued, d)
		}
	}
	if th.IsPR {
		if err := s.fillPR(ctx, p, &th, items); err != nil {
			return th, 0, err
		}
	}
	labels, err := s.Store.Labels(ctx, p.Repo.ID)
	for _, l := range labels {
		if !slices.Contains(th.Labels, l) {
			th.AddLabels = append(th.AddLabels, l)
		}
	}
	return th, 0, err
}

func (s *Server) issue(w http.ResponseWriter, r *http.Request, p *page) error {
	th, n, err := s.thread(r, p, r.PathValue("ref"))
	if err != nil {
		return err
	}
	if n != 0 {
		http.Redirect(w, r, fmt.Sprintf("/%s/issues/%d", p.Repo.FullName(), n), http.StatusSeeOther)
		return nil
	}
	p.Data = th
	return s.render(w, "issue", p)
}

func (s *Server) newIssueForm(w http.ResponseWriter, r *http.Request, p *page) error {
	labels, err := s.Store.Labels(r.Context(), p.Repo.ID)
	p.Data = labels
	if err != nil {
		return err
	}
	return s.render(w, "new", p)
}

// text validates a title or body from a form.
func text(r *http.Request, field string, required bool, limit int) (string, error) {
	v := strings.TrimSpace(strings.ReplaceAll(r.FormValue(field), "\r\n", "\n"))
	switch {
	case required && v == "":
		return "", badRequest(field + " is required")
	case utf8.RuneCountInString(v) > limit:
		return "", badRequest(fmt.Sprintf("%s is longer than %d characters", field, limit))
	}
	return v, nil
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request, p *page) error {
	ctx := r.Context()
	title, err := text(r, "title", true, 256)
	if err != nil {
		return err
	}
	body, err := text(r, "body", false, maxBody)
	if err != nil {
		return err
	}
	known, err := s.Store.Labels(ctx, p.Repo.ID)
	if err != nil {
		return err
	}
	c := store.Change{Author: p.User.Name, Title: title, Body: body}
	for _, l := range r.Form["label"] {
		if !slices.Contains(known, l) {
			return badRequest("unknown label " + l)
		}
		c.Labels = append(c.Labels, l)
	}
	ref, err := s.Store.Record(ctx, p.Repo.ID, p.User.ID, "issue", "", c)
	if err != nil {
		return err
	}
	http.Redirect(w, r, fmt.Sprintf("/%s/issues/%s", p.Repo.FullName(), ref), http.StatusSeeOther)
	return nil
}

// act queues a comment, label change, close or reopen, or edit on an issue.
func (s *Server) act(w http.ResponseWriter, r *http.Request, p *page) error {
	ctx := r.Context()
	th, n, err := s.thread(r, p, r.PathValue("ref"))
	if err != nil {
		return err
	}
	ref := th.Ref
	if n != 0 {
		ref = strconv.Itoa(n) // a new issue now on GitHub: act on its number
		if th, _, err = s.thread(r, p, ref); err != nil {
			return err
		}
	}
	c := store.Change{Author: p.User.Name}
	var kinds []string
	var changes []store.Change
	switch r.PathValue("action") {
	case "comment":
		if c.Body, err = text(r, "body", true, maxBody); err != nil {
			return err
		}
		kinds, changes = append(kinds, "comment"), append(changes, c)
	case "state":
		if c.State = r.FormValue("state"); c.State != "open" && c.State != "closed" || c.State == th.State {
			return badRequest("state must be open or closed, and differ from the current one")
		}
		kinds, changes = append(kinds, "state"), append(changes, c)
	case "label":
		c.Label, c.Add = r.FormValue("label"), r.FormValue("op") == "add"
		if c.Add && !slices.Contains(th.AddLabels, c.Label) || !c.Add && !slices.Contains(th.Labels, c.Label) {
			return badRequest("no such label to add or remove: " + c.Label)
		}
		kinds, changes = append(kinds, "label"), append(changes, c)
	case "review":
		if c, err = s.review(r, p, th); err != nil {
			return err
		}
		kinds, changes = append(kinds, "review"), append(changes, c)
	case "reply":
		if c.Body, err = text(r, "body", true, maxBody); err != nil {
			return err
		}
		c.Parent = r.FormValue("parent")
		if th.PR == nil || !slices.ContainsFunc(th.PR.Threads, func(t lineThread) bool { return t.Parent == c.Parent }) {
			return badRequest("no such line comment to reply to")
		}
		kinds, changes = append(kinds, "reply"), append(changes, c)
	case "merge":
		if !p.Mode.TakesWrites() {
			return store.ErrWrongMode
		}
		if err := s.merge(r, p, th); err != nil {
			return err
		}
	case "edit":
		title, err := text(r, "title", true, 256)
		if err != nil {
			return err
		}
		body, err := text(r, "body", false, maxBody)
		if err != nil {
			return err
		}
		if th.Local {
			if err := s.Store.EditLocal(ctx, p.Repo.ID, ref, title, body); err != nil {
				return badRequest(err.Error())
			}
			break
		}
		for field, v := range map[string][2]string{"title": {th.Title, title}, "body": {th.Body, body}} {
			if v[0] != v[1] {
				e := c
				e.Field, e.Value = field, v[1]
				kinds, changes = append(kinds, "edit"), append(changes, e)
			}
		}
	default:
		return store.ErrNotFound
	}
	for i := range kinds {
		if _, err := s.Store.Record(ctx, p.Repo.ID, p.User.ID, kinds[i], ref, changes[i]); err != nil {
			return err
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/%s/issues/%s", p.Repo.FullName(), ref), http.StatusSeeOther)
	return nil
}

// authorise links the caller's GitCove token to their GitHub account through GitHub's device
// flow (decision D3), streaming the code to enter and then the outcome as lines of text.
func (s *Server) authorise(w http.ResponseWriter, r *http.Request) {
	_, tok, _ := r.BasicAuth()
	t, err := s.Store.TokenFor(r.Context(), tok)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitCove"`)
		http.Error(w, "a GitCove token is required as the password", http.StatusUnauthorized)
		return
	}
	if s.Users == nil {
		http.Error(w, "this GitCove doesn't keep GitHub user tokens; the App posts everyone's work with an attribution line", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	login, err := s.Users.Authorise(r.Context(), t.ID, "token:"+t.Name, func(code, uri string) {
		fmt.Fprintf(w, "Open %s and enter the code %s\n", uri, code)
		flush()
	})
	if err != nil {
		s.Log.Warn("authorise failed", "token", t.ID, "err", err)
		fmt.Fprintf(w, "error: %v\n", err)
		return
	}
	fmt.Fprintf(w, "ok: GitCove will post your work from outages as %s\n", login)
}

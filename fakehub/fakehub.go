// Package fakehub is a GitHub stand-in for tests: App authentication, repo and branch listing,
// Git smart HTTP including pushes, and PR creation, with seeded faults so any failing run can be
// repeated exactly. Pushes are refused the way GitHub refuses them from an App: protected
// branches, and workflow trees that match no branch tip (spike W).
package fakehub

import (
	"bytes"
	"cmp"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// repoJSON is a repo object recorded from GitHub in milestone 0 (spikes/fixtures/setup/create-repo.body).
//
//go:embed testdata/repo.json
var repoJSON []byte

// Faults says how fakehub misbehaves. Rates are per request, between 0 and 1.
type Faults struct {
	APIDown, GitDown bool          // drop the connection on every API or Git request
	ErrorRate        float64       // answer 500
	TimeoutRate      float64       // hang until the client gives up
	CutRate          float64       // cut a Git pack response off partway
	LostRate         float64       // carry out a write (a push or any API write), then drop the connection unanswered
	RateLimitRate    float64       // refuse an API write with a secondary rate limit and Retry-After: 1
	ListingLag       time.Duration // hide each issue or comment created through the API from listings for up to this long
	Delay            time.Duration // answer every request, and the SSH greeting, this late
	SSHDown          bool          // drop SSH connections without a greeting
	TokenDown        bool          // answer 500 when the App asks for an installation token
}

// The App's OAuth client ID and secret, for the device flow and refreshing user tokens.
const (
	ClientID     = "Iv1.fakehub"
	ClientSecret = "fakehub-secret"
)

// PR is a pull request created through the API.
type PR struct {
	Number      int
	Head, Base  string
	Title, Body string
	State       string
	Author      string
	Merged      bool
}

// Push is one ref update a push made, from the post-receive log.
type Push struct{ Old, New, Ref string }

type Hub struct {
	*httptest.Server
	Org   string
	AppID string
	Key   *rsa.PrivateKey // the App's private key, for the client under test
	Root  string          // bare repos at Root/<org>/<name>.git
	SSH   string          // host:port of an SSH greeting, for health probes; it serves nothing else

	Personal bool // Org is a personal account: the App is found under /users, not /orgs

	NotModified atomic.Int64  // 304 responses sent
	AccessTTL   time.Duration // lifetime of user access tokens, default 8 hours

	mu        sync.Mutex
	rng       *mrand.Rand
	faults    Faults
	tokens    map[string]time.Time
	remaining int
	git       string
	hooks     string

	metas       map[string]*meta // issues, PRs, comments and labels by repo name
	nextComment int64
	devices     map[string]*device   // by device code
	userTokens  map[string]userToken // access token -> user
	refresh     map[string]string    // refresh token -> login
}

// New starts a fakehub whose faults are drawn from seed. It shuts down when the test ends.
func New(t testing.TB, seed uint64) *Hub {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	h := &Hub{Org: "fakeorg", AppID: "1", Key: key, Root: t.TempDir(), git: git, hooks: t.TempDir(),
		rng: mrand.New(mrand.NewPCG(seed, seed)), tokens: map[string]time.Time{}, remaining: 5000,
		metas: map[string]*meta{}, devices: map[string]*device{}, userTokens: map[string]userToken{}, refresh: map[string]string{}}
	for name, body := range map[string]string{"pre-receive": preReceive, "post-receive": "#!/bin/sh\ncat >> pushlog\n"} {
		if err := os.WriteFile(filepath.Join(h.hooks, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/{org}/installation", h.app(h.installation))
	mux.HandleFunc("GET /users/{org}/installation", h.app(h.installation))
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", h.app(h.accessToken))
	mux.HandleFunc("GET /installation/repositories", h.inst(h.repos))
	mux.HandleFunc("GET /rate_limit", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" { // GitHub answers this without signing in, too
			h.rateLimit(w, r)
			return
		}
		h.inst(h.rateLimit)(w, r)
	})
	mux.HandleFunc("GET /repos/{org}/{repo}/branches", h.inst(h.repo(h.branches)))
	mux.HandleFunc("GET /repos/{org}/{repo}/pulls", h.inst(h.repo(h.listPRs)))
	mux.HandleFunc("POST /repos/{org}/{repo}/pulls", h.inst(h.repo(h.createPR)))
	mux.HandleFunc("GET /repos/{org}/{repo}/pulls/comments", h.inst(h.repo(h.listReviewComments)))
	mux.HandleFunc("GET /repos/{org}/{repo}/pulls/{n}", h.inst(h.repo(h.getPR)))
	mux.HandleFunc("GET /repos/{org}/{repo}/pulls/{n}/comments", h.inst(h.repo(h.listReviewComments)))
	mux.HandleFunc("POST /repos/{org}/{repo}/pulls/{n}/comments/{id}/replies", h.inst(h.repo(h.createReply)))
	mux.HandleFunc("GET /repos/{org}/{repo}/pulls/{n}/reviews", h.inst(h.repo(h.listReviews)))
	mux.HandleFunc("POST /repos/{org}/{repo}/pulls/{n}/reviews", h.inst(h.repo(h.createReview)))
	mux.HandleFunc("GET /repos/{org}/{repo}/pulls/{n}/reviews/{id}/comments", h.inst(h.repo(h.listReviewComments)))
	mux.HandleFunc("GET /repos/{org}/{repo}/issues", h.inst(h.repo(h.listIssues)))
	mux.HandleFunc("POST /repos/{org}/{repo}/issues", h.inst(h.repo(h.createIssue)))
	mux.HandleFunc("GET /repos/{org}/{repo}/issues/comments", h.inst(h.repo(h.listComments)))
	mux.HandleFunc("GET /repos/{org}/{repo}/issues/{n}", h.inst(h.repo(h.getIssue)))
	mux.HandleFunc("PATCH /repos/{org}/{repo}/issues/{n}", h.inst(h.repo(h.editIssue)))
	mux.HandleFunc("GET /repos/{org}/{repo}/issues/{n}/comments", h.inst(h.repo(h.listComments)))
	mux.HandleFunc("POST /repos/{org}/{repo}/issues/{n}/comments", h.inst(h.repo(h.createComment)))
	mux.HandleFunc("POST /repos/{org}/{repo}/issues/{n}/labels", h.inst(h.repo(h.addLabels)))
	mux.HandleFunc("DELETE /repos/{org}/{repo}/issues/{n}/labels/{label}", h.inst(h.repo(h.removeLabel)))
	mux.HandleFunc("GET /repos/{org}/{repo}/labels", h.inst(h.repo(h.repoLabels)))
	mux.HandleFunc("GET /app", h.app(h.appInfo))
	mux.HandleFunc("GET /user", h.inst(h.user))
	mux.HandleFunc("POST /login/device/code", h.deviceCode)
	mux.HandleFunc("POST /login/oauth/access_token", h.oauthToken)
	mux.HandleFunc("/{org}/{repo}/{rest...}", h.serveGit)
	h.Server = httptest.NewServer(h.withFaults(mux))
	t.Cleanup(func() { h.CloseClientConnections(); h.Close() }) // hung timeout-fault requests would block Close
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	h.SSH = ln.Addr().String()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			f := h.faults
			h.mu.Unlock()
			go func() {
				defer conn.Close()
				time.Sleep(f.Delay)
				if !f.SSHDown {
					_, _ = io.WriteString(conn, "SSH-2.0-fakehub\r\n")
				}
			}()
		}
	}()
	return h
}

func (h *Hub) SetFaults(f Faults) { h.mu.Lock(); h.faults = f; h.mu.Unlock() }

func (h *Hub) withFaults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isGit := strings.Contains(r.URL.Path, ".git/")
		h.mu.Lock()
		f, roll, cut, limited := h.faults, h.rng.Float64(), h.rng.Float64(), h.rng.Float64()
		cutAt := 64 + h.rng.IntN(4096)
		h.mu.Unlock()
		time.Sleep(f.Delay)
		switch {
		case f.TokenDown && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
			return
		case isGit && f.GitDown, !isGit && f.APIDown:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		case roll < f.ErrorRate:
			http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
			return
		case roll < f.ErrorRate+f.TimeoutRate:
			io.Copy(io.Discard, r.Body) // net/http notices a closed connection only after the body is read
			<-r.Context().Done()
			return
		}
		if isGit && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git-upload-pack") && cut < f.CutRate {
			w = &cutWriter{w, cutAt}
		}
		apiWrite := !isGit && r.Method != "GET" && strings.HasPrefix(r.URL.Path, "/repos/")
		if apiWrite && limited < f.RateLimitRate {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`, http.StatusForbidden)
			return
		}
		write := apiWrite || r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git-receive-pack")
		if write && cut < f.LostRate {
			next.ServeHTTP(httptest.NewRecorder(), r) // done, but the answer never arrives
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cutWriter passes on n bytes, then drops the connection.
type cutWriter struct {
	http.ResponseWriter
	n int
}

func (c *cutWriter) Write(p []byte) (int, error) {
	if len(p) < c.n {
		c.n -= len(p)
		return c.ResponseWriter.Write(p)
	}
	c.ResponseWriter.Write(p[:c.n])
	c.ResponseWriter.(http.Flusher).Flush()
	panic(http.ErrAbortHandler) // net/http closes the connection without logging
}

// app requires an App JWT signed with Key.
func (h *Hub) app(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		var claims struct {
			Exp int64  `json:"exp"`
			Iss string `json:"iss"`
		}
		ok := len(parts) == 3
		if ok {
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			body, _ := base64.RawURLEncoding.DecodeString(parts[1])
			ok = rsa.VerifyPKCS1v15(&h.Key.PublicKey, crypto.SHA256, sum[:], sig) == nil &&
				json.Unmarshal(body, &claims) == nil && claims.Iss == h.AppID && claims.Exp > time.Now().Unix()
		}
		if !ok {
			h.json(w, r, http.StatusUnauthorized, map[string]any{"message": "A JSON web token could not be decoded"})
			return
		}
		next(w, r)
	}
}

// inst requires a live installation token or user token.
func (h *Hub) inst(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.actor(r) == "" {
			h.json(w, r, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		next(w, r)
	}
}

func (h *Hub) validToken(tok string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	exp, ok := h.tokens[tok]
	return ok && time.Now().Before(exp)
}

func (h *Hub) installation(w http.ResponseWriter, r *http.Request) {
	kind := "Organization"
	if h.Personal {
		kind = "User"
	}
	// GitHub's /users/{name}/installation also finds org installations; /orgs/{name} only finds orgs.
	if r.PathValue("org") != h.Org || (h.Personal && strings.HasPrefix(r.URL.Path, "/orgs/")) {
		h.json(w, r, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	h.json(w, r, http.StatusOK, map[string]any{"id": 4242, "app_id": 1, "target_type": kind,
		"account": map[string]any{"login": h.Org, "type": kind}})
}

func (h *Hub) accessToken(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 18)
	rand.Read(b)
	tok, exp := "ghs_"+hex.EncodeToString(b), time.Now().Add(time.Hour).UTC()
	h.mu.Lock()
	h.tokens[tok] = exp
	h.mu.Unlock()
	h.json(w, r, http.StatusCreated, map[string]any{"token": tok, "expires_at": exp.Format(time.RFC3339),
		"permissions": map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}, "repository_selection": "selected"})
}

func (h *Hub) repos(w http.ResponseWriter, r *http.Request) {
	dirs, _ := filepath.Glob(filepath.Join(h.Root, h.Org, "*.git"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 || perPage > 100 {
		perPage = 30
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	var list []map[string]any
	for i, dir := range dirs {
		if i < (page-1)*perPage || i >= page*perPage {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(dir), ".git")
		head, _ := exec.Command(h.git, "-C", dir, "symbolic-ref", "--short", "HEAD").Output()
		var repo map[string]any
		_ = json.Unmarshal(repoJSON, &repo) // embedded fixture, known good
		repo["id"], repo["name"], repo["full_name"] = RepoID(name), name, h.Org+"/"+name
		repo["default_branch"] = strings.TrimSpace(string(head))
		repo["owner"].(map[string]any)["login"] = h.Org
		repo["clone_url"] = h.URL + "/" + h.Org + "/" + name + ".git"
		list = append(list, repo)
	}
	if page*perPage < len(dirs) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/installation/repositories?per_page=%d&page=%d>; rel="next"`, h.URL, perPage, page+1))
	}
	h.json(w, r, http.StatusOK, map[string]any{"total_count": len(dirs), "repositories": list})
}

func (h *Hub) rateLimit(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	core := map[string]any{"limit": 5000, "remaining": h.remaining, "used": 5000 - h.remaining, "reset": time.Now().Add(time.Hour).Unix()}
	h.mu.Unlock()
	h.json(w, r, http.StatusOK, map[string]any{"resources": map[string]any{"core": core}, "rate": core})
}

// json writes v with GitHub's ETag and rate-limit headers, answering 304 when If-None-Match matches.
func (h *Hub) json(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, _ := json.Marshal(v)
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	h.mu.Lock()
	notModified := status == http.StatusOK && r.Header.Get("If-None-Match") == etag
	if !notModified {
		h.remaining = max(h.remaining-1, 0)
	}
	rem := h.remaining
	h.mu.Unlock()
	hd := w.Header()
	hd.Set("X-RateLimit-Limit", "5000")
	hd.Set("X-RateLimit-Remaining", strconv.Itoa(rem))
	hd.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	hd.Set("X-RateLimit-Resource", "core")
	hd.Set("ETag", etag)
	if notModified {
		h.NotModified.Add(1)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hd.Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

func (h *Hub) serveGit(w http.ResponseWriter, r *http.Request) {
	_, pass, _ := r.BasicAuth()
	if !h.validToken(pass) {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(w, "Invalid username or token.", http.StatusUnauthorized)
		return
	}
	(&cgi.Handler{Path: h.git, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + h.Root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true",
			"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=" + h.hooks}}).ServeHTTP(w, r)
}

// preReceive refuses pushes to branches listed in the repo's "protected" file, and pushes whose
// .github/workflows tree matches no current branch tip, as GitHub does without the workflows permission.
const preReceive = `#!/bin/sh
while read old new ref; do
  b=${ref#refs/heads/}
  if [ -f protected ] && grep -qxF "$b" protected; then
    echo "error: GH006: Protected branch update failed for $ref." >&2; exit 1
  fi
  case "$new" in *[!0]*) ;; *) continue ;; esac
  want=$(git rev-parse -q --verify "$new:.github/workflows" || echo none)
  ok=
  for tip in $(git for-each-ref --format='%(objectname)' refs/heads/); do
    [ "$(git rev-parse -q --verify "$tip:.github/workflows" || echo none)" = "$want" ] && { ok=1; break; }
  done
  [ -n "$ok" ] || { echo "refusing to allow a GitHub App to create or update workflow without workflows permission" >&2; exit 1; }
done
`

// repo answers 404 for repos fakehub doesn't have.
func (h *Hub) repo(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(h.Dir(r.PathValue("repo"))); r.PathValue("org") != h.Org || err != nil {
			h.json(w, r, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		next(w, r)
	}
}

func (h *Hub) protected(name string) map[string]bool {
	b, _ := os.ReadFile(filepath.Join(h.Dir(name), "protected"))
	out := map[string]bool{}
	for _, l := range strings.Fields(string(b)) {
		out[l] = true
	}
	return out
}

func (h *Hub) branches(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	prot := h.protected(name)
	out, _ := exec.Command(h.git, "-C", h.Dir(name), "for-each-ref", "--format=%(refname:lstrip=2) %(objectname)", "refs/heads/").Output()
	list := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if b, sha, ok := strings.Cut(line, " "); ok {
			list = append(list, map[string]any{"name": b, "commit": map[string]any{"sha": sha}, "protected": prot[b]})
		}
	}
	h.json(w, r, http.StatusOK, list)
}

func (h *Hub) listPRs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := r.PathValue("repo")
	_, head, _ := strings.Cut(q.Get("head"), ":")
	state := cmp.Or(q.Get("state"), "open")
	h.mu.Lock()
	var list []*Issue
	for _, pr := range h.meta(name).issues {
		if pr.PR && !time.Now().Before(pr.visible) && (head == "" || pr.Head == head) && (state == "all" || pr.State == state) {
			c := *pr
			list = append(list, &c)
		}
	}
	h.mu.Unlock()
	h.listing(w, r, len(list), func(k int) time.Time { return list[k].Created }, func(k int) time.Time { return list[k].Updated },
		func(k int) map[string]any { return h.prJSON(name, list[k]) })
}

func (h *Hub) createPR(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	var in struct{ Title, Head, Base, Body string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Title == "" {
		h.json(w, r, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
		return
	}
	if i := strings.Index(in.Head, ":"); i >= 0 {
		in.Head = in.Head[i+1:]
	}
	fail := func(msg string) {
		h.json(w, r, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed",
			"errors": []map[string]any{{"resource": "PullRequest", "code": "custom", "message": msg}}})
	}
	ref := func(b string) string {
		out, _ := exec.Command(h.git, "-C", h.Dir(name), "rev-parse", "-q", "--verify", "refs/heads/"+b).Output()
		return strings.TrimSpace(string(out))
	}
	head, base := ref(in.Head), ref(in.Base)
	if head == "" || base == "" {
		fail("head or base doesn't exist")
		return
	}
	if exec.Command(h.git, "-C", h.Dir(name), "merge-base", "--is-ancestor", head, base).Run() == nil {
		fail("No commits between " + in.Base + " and " + in.Head)
		return
	}
	author := h.actor(r)
	h.mu.Lock()
	m := h.meta(name)
	for _, pr := range m.issues {
		if pr.PR && pr.State == "open" && pr.Head == in.Head && pr.Base == in.Base {
			h.mu.Unlock()
			fail(fmt.Sprintf("A pull request already exists for %s:%s.", h.Org, in.Head))
			return
		}
	}
	t := now()
	pr := &Issue{Number: len(m.issues) + 1, PR: true, Head: in.Head, Base: in.Base, BaseSHA: base, Title: in.Title, Body: in.Body, FirstBody: in.Body, State: "open",
		Author: author, Created: t, Updated: t, visible: h.lag()}
	m.issues = append(m.issues, pr)
	c := *pr
	h.mu.Unlock()
	h.json(w, r, http.StatusCreated, h.prJSON(name, &c))
}

// PRs returns the PRs created in repo name.
func (h *Hub) PRs(name string) []PR {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []PR
	for _, i := range h.meta(name).issues {
		if i.PR {
			out = append(out, PR{Number: i.Number, Head: i.Head, Base: i.Base, Title: i.Title, Body: i.Body, State: i.State, Author: i.Author, Merged: i.Merged})
		}
	}
	return out
}

// Protect makes pushes to branch in repo name fail as GitHub's branch protection would, and
// sets the branch listing's protected flag.
func (h *Hub) Protect(t testing.TB, name, branch string) {
	f, err := os.OpenFile(filepath.Join(h.Dir(name), "protected"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = fmt.Fprintln(f, branch)
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		t.Fatal(err)
	}
}

// Pushes returns every ref update made by a push to repo name, in order.
func (h *Hub) Pushes(name string) []Push {
	b, _ := os.ReadFile(filepath.Join(h.Dir(name), "pushlog"))
	var out []Push
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if f := strings.Fields(line); len(f) == 3 {
			out = append(out, Push{f[0], f[1], f[2]})
		}
	}
	return out
}

// RepoID is the GitHub ID fakehub gives the repo called name.
func RepoID(name string) int64 {
	f := fnv.New32a()
	f.Write([]byte(name))
	return int64(f.Sum32()) + 1000
}

// Dir is the bare repo for name.
func (h *Hub) Dir(name string) string { return filepath.Join(h.Root, h.Org, name+".git") }

// AddRepo creates a repo with main, a feature branch, an annotated tag v1 and refs/pull/1/head.
func (h *Hub) AddRepo(t testing.TB, name string) {
	dir := h.Dir(name)
	h.gitT(t, "", "init", "--bare", "-q", "-b", "main", dir)
	h.Commit(t, name, "main")
	h.gitT(t, dir, "tag", "-a", "-m", "v1", "v1", "main")
	h.gitT(t, dir, "update-ref", "refs/heads/feature", h.Commit(t, name, "main"))
	h.gitT(t, dir, "update-ref", "refs/pull/1/head", h.Commit(t, name, "feature"))
}

// Commit adds a commit with a few KB of random content on top of branch (creating it) and returns its ID.
func (h *Hub) Commit(t testing.TB, name, branch string) string {
	dir := h.Dir(name)
	b := make([]byte, 16<<10)
	rand.Read(b) // incompressible, so pack responses are big enough to cut
	blob := h.gitIn(t, dir, base64.StdEncoding.EncodeToString(b), "hash-object", "-w", "--stdin")
	tree := h.gitIn(t, dir, fmt.Sprintf("100644 blob %s\tdata-%s\n", blob, blob[:8]), "mktree")
	for try := 1; ; try++ { // compare-and-swap, so a push landing meanwhile isn't undone; "" as the old value means a new branch
		args := []string{"commit-tree", tree, "-m", "commit " + blob[:8]}
		out, err := exec.Command(h.git, "-C", dir, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
		parent := strings.TrimSpace(string(out))
		if err == nil {
			args = append(args, "-p", parent)
		}
		sha := h.gitT(t, dir, args...)
		if try == 50 {
			h.gitT(t, dir, "update-ref", "refs/heads/"+branch, sha, parent) // fails the test with git's reason
			return sha
		}
		if exec.Command(h.git, "-C", dir, "update-ref", "refs/heads/"+branch, sha, parent).Run() == nil {
			return sha
		}
	}
}

// CommitFile commits path with content on top of branch's tree and returns the commit ID.
func (h *Hub) CommitFile(t testing.TB, name, branch, path, content string) string {
	dir := h.Dir(name)
	parent := h.gitT(t, dir, "rev-parse", "refs/heads/"+branch)
	blob := h.gitIn(t, dir, content, "hash-object", "-w", "--stdin")
	idx := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")}
	h.gitEnv(t, dir, idx, "", "read-tree", parent)
	h.gitEnv(t, dir, idx, "", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	tree := h.gitEnv(t, dir, idx, "", "write-tree")
	sha := h.gitT(t, dir, "commit-tree", tree, "-p", parent, "-m", "change "+path)
	h.gitT(t, dir, "update-ref", "refs/heads/"+branch, sha, parent)
	return sha
}

// Refs returns the repo's refs and the objects they point to.
func (h *Hub) Refs(t testing.TB, name string) map[string]string {
	refs := map[string]string{}
	for _, line := range strings.Split(h.gitT(t, h.Dir(name), "for-each-ref", "--format=%(refname) %(objectname)"), "\n") {
		if ref, sha, ok := strings.Cut(line, " "); ok {
			refs[ref] = sha
		}
	}
	return refs
}

func (h *Hub) gitT(t testing.TB, dir string, args ...string) string {
	return h.gitIn(t, dir, "", args...)
}

func (h *Hub) gitIn(t testing.TB, dir, stdin string, args ...string) string {
	t.Helper()
	return h.gitEnv(t, dir, nil, stdin, args...)
}

func (h *Hub) gitEnv(t testing.TB, dir string, env []string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(h.git, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(append(os.Environ(), env...), "GIT_AUTHOR_NAME=fakehub", "GIT_AUTHOR_EMAIL=fakehub@example.invalid",
		"GIT_COMMITTER_NAME=fakehub", "GIT_COMMITTER_EMAIL=fakehub@example.invalid")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// Package fakehub is a GitHub stand-in for tests: App authentication, repo listing and Git smart
// HTTP, with seeded faults so any failing run can be repeated exactly.
package fakehub

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	mrand "math/rand/v2"
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
	APIDown, GitDown bool    // drop the connection on every API or Git request
	ErrorRate        float64 // answer 500
	TimeoutRate      float64 // hang until the client gives up
	CutRate          float64 // cut a Git pack response off partway
}

type Hub struct {
	*httptest.Server
	Org   string
	AppID string
	Key   *rsa.PrivateKey // the App's private key, for the client under test
	Root  string          // bare repos at Root/<org>/<name>.git

	NotModified atomic.Int64 // 304 responses sent

	mu        sync.Mutex
	rng       *mrand.Rand
	faults    Faults
	tokens    map[string]time.Time
	remaining int
	git       string
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
	h := &Hub{Org: "fakeorg", AppID: "1", Key: key, Root: t.TempDir(), git: git,
		rng: mrand.New(mrand.NewPCG(seed, seed)), tokens: map[string]time.Time{}, remaining: 5000}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/{org}/installation", h.app(h.installation))
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", h.app(h.accessToken))
	mux.HandleFunc("GET /installation/repositories", h.inst(h.repos))
	mux.HandleFunc("GET /rate_limit", h.inst(h.rateLimit))
	mux.HandleFunc("/{org}/{repo}/{rest...}", h.serveGit)
	h.Server = httptest.NewServer(h.withFaults(mux))
	t.Cleanup(func() { h.CloseClientConnections(); h.Close() }) // hung timeout-fault requests would block Close
	return h
}

func (h *Hub) SetFaults(f Faults) { h.mu.Lock(); h.faults = f; h.mu.Unlock() }

func (h *Hub) withFaults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isGit := strings.Contains(r.URL.Path, ".git/")
		h.mu.Lock()
		f, roll, cut := h.faults, h.rng.Float64(), h.rng.Float64()
		cutAt := 64 + h.rng.IntN(4096)
		h.mu.Unlock()
		switch {
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

// inst requires a live installation token.
func (h *Hub) inst(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.validToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
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
	if r.PathValue("org") != h.Org {
		h.json(w, r, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	h.json(w, r, http.StatusOK, map[string]any{"id": 4242, "app_id": 1, "target_type": "Organization",
		"account": map[string]any{"login": h.Org, "type": "Organization"}})
}

func (h *Hub) accessToken(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 18)
	rand.Read(b)
	tok, exp := "ghs_"+hex.EncodeToString(b), time.Now().Add(time.Hour).UTC()
	h.mu.Lock()
	h.tokens[tok] = exp
	h.mu.Unlock()
	h.json(w, r, http.StatusCreated, map[string]any{"token": tok, "expires_at": exp.Format(time.RFC3339),
		"permissions": map[string]string{"contents": "read", "metadata": "read"}, "repository_selection": "selected"})
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
		Env: []string{"GIT_PROJECT_ROOT=" + h.Root, "GIT_HTTP_EXPORT_ALL=1"}}).ServeHTTP(w, r)
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
	args := []string{"commit-tree", tree, "-m", "commit " + blob[:8]}
	if parent, err := exec.Command(h.git, "-C", dir, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output(); err == nil {
		args = append(args, "-p", strings.TrimSpace(string(parent)))
	}
	sha := h.gitT(t, dir, args...)
	h.gitT(t, dir, "update-ref", "refs/heads/"+branch, sha)
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
	cmd := exec.Command(h.git, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=fakehub", "GIT_AUTHOR_EMAIL=fakehub@example.invalid",
		"GIT_COMMITTER_NAME=fakehub", "GIT_COMMITTER_EMAIL=fakehub@example.invalid")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

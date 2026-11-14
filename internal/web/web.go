// Package web is GitCove's HTTP front: Git over HTTP for developers, the web interface (ui.go),
// and the /status endpoint.
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tabaak/gitcove/internal/auth"
	"github.com/tabaak/gitcove/internal/gitstore"
	"github.com/tabaak/gitcove/internal/health"
	"github.com/tabaak/gitcove/internal/store"
)

type Server struct {
	Store  *store.Store
	Git    *gitstore.Store
	Health *health.Checker
	Users  *auth.Users // nil if GitCove doesn't keep GitHub user tokens
	Log    *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("POST /api/authorise", s.authorise)
	mux.HandleFunc("GET /{$}", s.ui(s.home))
	mux.HandleFunc("GET /{owner}/{repo}/issues", s.ui(s.issues))
	mux.HandleFunc("GET /{owner}/{repo}/issues/new", s.ui(s.newIssueForm))
	mux.HandleFunc("POST /{owner}/{repo}/issues", s.ui(s.createIssue))
	mux.HandleFunc("GET /{owner}/{repo}/issues/{ref}", s.ui(s.issue))
	mux.HandleFunc("GET /{owner}/{repo}/issues/{ref}/files", s.ui(s.files))
	mux.HandleFunc("GET /{owner}/{repo}/branches", s.ui(s.branches))
	mux.HandleFunc("GET /{owner}/{repo}/compare", s.ui(s.compare))
	mux.HandleFunc("POST /{owner}/{repo}/pulls", s.ui(s.createPull))
	mux.HandleFunc("POST /{owner}/{repo}/issues/{ref}/{action}", s.ui(s.act))
	mux.HandleFunc("/{owner}/{repo}/{rest...}", s.git)
	return mux
}

// git serves a repo to a GitCove token holder with access to it. The token is the Basic auth password.
func (s *Server) git(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, tok, _ := r.BasicAuth()
	valid, err := s.Store.TokenValid(ctx, tok)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !valid {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitCove"`)
		http.Error(w, "a GitCove token is required as the password", http.StatusUnauthorized)
		return
	}
	repo, err := s.Store.RepoByName(ctx, r.PathValue("owner"), strings.TrimSuffix(r.PathValue("repo"), ".git"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	pusher, err := s.Store.TokenAccess(ctx, tok, repo.ID)
	if err != nil {
		s.fail(w, err)
		return
	} else if pusher == "" {
		http.NotFound(w, r) // same answer as a missing repo, so tokens can't probe for repo names
		return
	}
	rest := "/" + r.PathValue("rest")
	if rest == "/git-receive-pack" || r.URL.Query().Get("service") == "git-receive-pack" {
		// A quick refusal; the pre-receive hook checks the mode again, atomically with recording the push.
		if mode, err := s.Store.Mode(ctx); err != nil {
			s.fail(w, err)
			return
		} else if mode != store.Failover {
			http.Error(w, "GitCove accepts pushes only during failover; push to GitHub", http.StatusForbidden)
			return
		}
	}
	s.Git.Handler(repo.GitHubID, rest, "COVE_DB="+s.Store.Path(), fmt.Sprintf("COVE_REPO_ID=%d", repo.ID), "COVE_PUSHER="+pusher).ServeHTTP(w, r)
}

// status is public, so it shows counts and health but no repo names or errors.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	repos, err := s.Store.Repos(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	st := struct {
		Mode         string `json:"mode"`
		Repos        int    `json:"repos"`
		NeverSynced  int    `json:"repos_never_synced"`
		SyncErrors   int    `json:"repos_with_sync_errors"`
		OldestSynced string `json:"oldest_synced_at,omitempty"` // the age of the copy
		GitHub       struct {
			API health.Result `json:"api"`
			Git health.Result `json:"git"`
		} `json:"github"`
	}{Repos: len(repos)}
	mode, err := s.Store.Mode(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	st.Mode = string(mode)
	for _, r := range repos {
		switch {
		case r.SyncedAt == "":
			st.NeverSynced++
		case st.OldestSynced == "" || r.SyncedAt < st.OldestSynced:
			st.OldestSynced = r.SyncedAt
		}
		if r.SyncError != "" {
			st.SyncErrors++
		}
	}
	st.GitHub.API, st.GitHub.Git = s.Health.Status()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

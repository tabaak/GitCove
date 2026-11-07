package ghclient

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tabaak/gitcove/fakehub"
)

func TestAppAgainstFakehub(t *testing.T) {
	hub := fakehub.New(t, 1)
	for i := range 3 {
		hub.AddRepo(t, fmt.Sprintf("r%d", i))
	}
	ctx := context.Background()
	a := &App{API: hub.URL, AppID: hub.AppID, Key: hub.Key, Org: hub.Org, HTTP: hub.Client()}

	repos, err := a.Repos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 3 || repos[0].Owner.Login != hub.Org || repos[0].DefaultBranch != "main" || repos[0].ID != fakehub.RepoID("r0") {
		t.Fatalf("repos = %+v", repos)
	}
	if _, err := a.Repos(ctx); err != nil {
		t.Fatal(err)
	}
	if hub.NotModified.Load() != 1 {
		t.Fatalf("second listing got %d 304s, want 1", hub.NotModified.Load())
	}
	tok1, _ := a.GitToken(ctx)
	if tok2, _ := a.GitToken(ctx); tok1 != tok2 {
		t.Fatal("installation token not cached")
	}
	if err := a.Probe(ctx); err != nil {
		t.Fatal(err)
	}

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	a.Key, a.token = other, "" // wrong key: fakehub must refuse the JWT
	var se *StatusError
	if _, err := a.GitToken(ctx); !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Fatalf("wrong key: err = %v", err)
	}
}

func TestPagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/installation/repositories?page=2>; rel="next", <%s/x?page=2>; rel="last"`, srv.URL, srv.URL))
			fmt.Fprint(w, `{"repositories":[{"id":1}]}`)
			return
		}
		fmt.Fprint(w, `{"repositories":[{"id":2}]}`)
	}))
	defer srv.Close()
	a := &App{API: srv.URL, HTTP: srv.Client(), token: "t", tokenExp: time.Now().Add(time.Hour)}
	repos, err := a.Repos(context.Background())
	if err != nil || len(repos) != 2 || repos[1].ID != 2 {
		t.Fatalf("repos = %+v, err = %v", repos, err)
	}
}

func TestRetryAfterBlocksAllRequests(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit"}`)
	}))
	defer srv.Close()
	a := &App{API: srv.URL, HTTP: srv.Client(), token: "t", tokenExp: time.Now().Add(time.Hour)}
	var se *StatusError
	if err := a.Probe(context.Background()); !errors.As(err, &se) || se.Status != http.StatusForbidden {
		t.Fatalf("first call: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Probe(ctx); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second call: %v, want ErrRateLimited", err)
	}
	if calls != 1 {
		t.Fatalf("blocked client still sent requests: %d calls", calls)
	}
}

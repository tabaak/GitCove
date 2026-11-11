package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tabaak/gitcove/internal/store"
)

func TestUI(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "cove.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	repoID, err := st.UpsertRepo(ctx, 1, "org", "app", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertRepo(ctx, 2, "org", "secret", "main"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveIssues(ctx, repoID, "c", "2026-01-01T00:00:00Z", []store.Issue{{Number: 1, Title: "old", Body: "b", State: "open",
		Author: "carol", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLabels(ctx, repoID, []string{"bug"}); err != nil {
		t.Fatal(err)
	}
	_, tok, err := st.CreateToken(ctx, "test", "ben", []string{"org/app"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{Store: st, Log: slog.New(slog.DiscardHandler)}).Handler())
	defer srv.Close()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path string, form url.Values, hdr ...string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	if resp, err := noRedirect.Get(srv.URL + "/"); err != nil || resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("signed out: %v %v", resp, err)
	}
	resp, _ := noRedirect.PostForm(srv.URL+"/login", url.Values{"token": {tok}})
	if c := resp.Cookies(); len(c) != 1 || c[0].Value != tok || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("login cookie: %+v", c)
	}
	if _, body := do("GET", "/", nil); !strings.Contains(body, "org/app") || strings.Contains(body, "org/secret") {
		t.Fatalf("home: %s", body)
	}
	if resp, _ := do("GET", "/org/secret/issues", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("repo without access: %d", resp.StatusCode)
	}
	if resp, _ := do("POST", "/org/app/issues", url.Values{"title": {"x"}}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("write outside failover: %d", resp.StatusCode)
	}

	if err := st.SetMode(ctx, "test", store.Failover); err != nil {
		t.Fatal(err)
	}
	resp, _ = do("POST", "/org/app/issues", url.Values{"title": {"<b>new</b>"}, "body": {"<script>x</script> see #1"}, "label": {"bug"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/org/app/issues/L-1" {
		t.Fatalf("create: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, body := do("GET", "/org/app/issues/L-1", nil); !strings.Contains(body, "&lt;script&gt;") || strings.Contains(body, "<script>x") ||
		!strings.Contains(body, "queued") {
		t.Fatalf("new issue page: %s", body)
	}
	if resp, _ := do("POST", "/org/app/issues", url.Values{"title": {"x"}}, "Origin", "https://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", resp.StatusCode)
	}
	if resp, _ := do("POST", "/org/app/issues/1/label", url.Values{"op": {"add"}, "label": {"nope"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown label: %d", resp.StatusCode)
	}
	for _, a := range []struct {
		path string
		form url.Values
	}{
		{"/org/app/issues/L-1/comment", url.Values{"body": {"on the new one"}}},
		{"/org/app/issues/L-1/edit", url.Values{"title": {"renamed"}, "body": {"b"}}},
		{"/org/app/issues/1/comment", url.Values{"body": {"see #L-1"}}},
		{"/org/app/issues/1/state", url.Values{"state": {"closed"}}},
		{"/org/app/issues/1/label", url.Values{"op": {"add"}, "label": {"bug"}}},
		{"/org/app/issues/1/edit", url.Values{"title": {"new title"}, "body": {"b"}}},
	} {
		if resp, body := do("POST", a.path, a.form); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", a.path, resp.StatusCode, body)
		}
	}
	items, _ := st.MetaItems(ctx, repoID, "pending")
	var got []string
	for _, it := range items {
		got = append(got, it.Kind+" "+it.Issue+" base="+it.Base)
	}
	want := "issue L-1 base=,comment L-1 base=,comment 1 base=,state 1 base=open,label 1 base=,edit 1 base=old"
	if strings.Join(got, ",") != want {
		t.Fatalf("items:\n%s\nwant:\n%s", strings.Join(got, ","), want)
	}
	if items[0].Change.Title != "renamed" {
		t.Errorf("the new issue wasn't edited in place: %+v", items[0].Change)
	}
	if _, body := do("GET", "/org/app/issues/1", nil); !strings.Contains(body, "new title") || !strings.Contains(body, "closed by ben, queued") {
		t.Errorf("#1 page doesn't show queued changes: %s", body)
	}
}

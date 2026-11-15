package main

// End to end: the real cove binary (sync, token CLI, serve) against fakehub.

import (
	"bufio"
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tabaak/gitcove/fakehub"
)

type e2e struct {
	hub  *fakehub.Hub
	bin  string
	addr string
	env  []string
}

// start builds cove and syncs fakehub's "app" repo into a fresh data directory.
func start(t *testing.T) *e2e {
	hub := fakehub.New(t, 1)
	hub.AddRepo(t, "app")
	dir := t.TempDir()
	bin := filepath.Join(dir, "cove")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	keyFile := filepath.Join(dir, "app.pem")
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(hub.Key)})
	if err := os.WriteFile(keyFile, pemKey, 0o600); err != nil {
		t.Fatal(err)
	}
	secretFile, userKeyFile := filepath.Join(dir, "client-secret"), filepath.Join(dir, "user.key")
	if err := errors.Join(os.WriteFile(secretFile, []byte(fakehub.ClientSecret+"\n"), 0o600),
		os.WriteFile(userKeyFile, []byte(strings.Repeat("0f", 32)+"\n"), 0o600)); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	env := append(os.Environ(), "COVE_ORG="+hub.Org, "COVE_APP_ID="+hub.AppID, "COVE_APP_KEY_FILE="+keyFile,
		"COVE_APP_CLIENT_ID="+fakehub.ClientID, "COVE_APP_CLIENT_SECRET_FILE="+secretFile, "COVE_USER_KEY_FILE="+userKeyFile,
		"COVE_DATA="+filepath.Join(dir, "data"), "COVE_LISTEN="+addr, "COVE_GITHUB_API="+hub.URL, "COVE_GITHUB_GIT="+hub.URL, "COVE_GITHUB_SSH="+hub.SSH,
		"COVE_HEALTH_INTERVAL=200ms", "COVE_SYNC_INTERVAL=1h", "COVE_REPLAY_INTERVAL=200ms", "GIT_TERMINAL_PROMPT=0")
	x := &e2e{hub, bin, addr, env}
	if _, e, err := x.cove("sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, e)
	}
	return x
}

func (x *e2e) cove(args ...string) (string, string, error) {
	var o, e bytes.Buffer
	c := exec.Command(x.bin, args...)
	c.Env, c.Stdout, c.Stderr = x.env, &o, &e
	err := c.Run()
	return strings.TrimSpace(o.String()), e.String(), err
}

// serve starts "cove serve" and returns its log; it stops when the test ends.
func (x *e2e) serve(t *testing.T) (*bytes.Buffer, chan error, *exec.Cmd) {
	srv := exec.Command(x.bin, "serve")
	srv.Env = x.env
	var srvLog bytes.Buffer
	srv.Stderr = &srvLog
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	t.Cleanup(func() { srv.Process.Kill() })
	return &srvLog, done, srv
}

type status struct {
	Mode   string
	Repos  int
	GitHub map[string]struct{ OK bool }
}

func (x *e2e) waitStatus(t *testing.T, srvLog *bytes.Buffer, what string, ok func(status) bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		resp, err := http.Get("http://" + x.addr + "/status")
		if err != nil {
			continue
		}
		var s status
		json.NewDecoder(resp.Body).Decode(&s)
		resp.Body.Close()
		if ok(s) {
			return
		}
	}
	t.Fatalf("timed out waiting for %s\nserver log:\n%s", what, srvLog.String())
}

func TestBinaryEndToEnd(t *testing.T) {
	x := start(t)
	hub, addr, cove := x.hub, x.addr, x.cove
	if _, _, err := cove("token", "create", "-name", "x", "-repo", hub.Org+"/nope"); err == nil {
		t.Error("token for an unsynced repo was accepted")
	}
	tok, e, err := cove("token", "create", "-name", "alice", "-repo", hub.Org+"/app")
	if err != nil || tok == "" {
		t.Fatalf("token create: %v\n%s", err, e)
	}
	if list, _, _ := cove("token", "list"); !strings.Contains(list, "alice") {
		t.Fatalf("token list lacks alice:\n%s", list)
	}

	srvLog, done, srv := x.serve(t)
	waitStatus := func(what string, ok func(status) bool) { t.Helper(); x.waitStatus(t, srvLog, what, ok) }
	clone := func(tok string) error {
		out, err := exec.Command("git", "clone", "-q", "http://x:"+tok+"@"+addr+"/"+hub.Org+"/app.git", t.TempDir()+"/c").CombinedOutput()
		if err != nil {
			t.Logf("clone: %s", out)
		}
		return err
	}

	waitStatus("GitHub up", func(s status) bool { return s.Repos == 1 && s.GitHub["api"].OK && s.GitHub["git-https"].OK })
	if err := clone(tok); err != nil {
		t.Fatalf("clone: %v", err)
	}

	hub.SetFaults(fakehub.Faults{APIDown: true, GitDown: true})
	waitStatus("GitHub down", func(s status) bool { return !s.GitHub["api"].OK && !s.GitHub["git-https"].OK })
	if err := clone(tok); err != nil {
		t.Fatalf("clone with GitHub down: %v", err)
	}

	if _, e, err := cove("token", "revoke", "1"); err != nil {
		t.Fatalf("revoke: %v\n%s", err, e)
	}
	if err := clone(tok); err == nil {
		t.Fatal("clone with a revoked token succeeded")
	}

	srv.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve exit: %v\n%s", err, srvLog.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop on SIGTERM")
	}
}

// TestFailoverEndToEnd follows the outage story with the real binary: a developer links their
// GitHub account with `cove client authorise`, their agent sends their clone of the GitHub URL to
// GitCove once failover starts, a push lands during failover and they open an issue in the web
// interface. GitCove then restarts, and the agent changes nothing while it can't reach it. After
// `failover end` the agent switches back, and after `replay approve` the server replays it all:
// a new branch directly, main as a PR, and the issue under their GitHub name.
func TestFailoverEndToEnd(t *testing.T) {
	x := start(t)
	tok, e, err := x.cove("token", "create", "-name", "ben", "-repo", x.hub.Org+"/app")
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, e)
	}
	srvLog, done, srv := x.serve(t)
	x.waitStatus(t, srvLog, "server up", func(s status) bool { return s.Mode == "normal" && s.Repos == 1 })

	// A developer machine: its own global Git config with a credential helper.
	home := t.TempDir()
	gitcfg := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(gitcfg, []byte("[credential]\n\thelper = store --file "+filepath.Join(home, "creds")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	devEnv := append(os.Environ(), "GIT_CONFIG_GLOBAL="+gitcfg, "GIT_TERMINAL_PROMPT=0", "COVE_TOKEN="+tok,
		"GIT_AUTHOR_NAME=ben", "GIT_AUTHOR_EMAIL=ben@example.invalid", "GIT_COMMITTER_NAME=ben", "GIT_COMMITTER_EMAIL=ben@example.invalid")
	run := func(dir, name string, args ...string) string {
		t.Helper()
		c := exec.Command(name, args...)
		c.Dir, c.Env = dir, devEnv
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s\nserver log:\n%s", name, args, err, out, srvLog)
		}
		return strings.TrimSpace(string(out))
	}
	run("", x.bin, "client", "setup", "-url", "http://"+x.addr, "-org", x.hub.Org, "-agent=false") // not a service on the test machine
	agent := exec.Command(x.bin, "client", "agent", "-every", "100ms")
	agent.Env = devEnv
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Process.Kill(); _ = agent.Wait() })
	ruleOn := func() bool { cfg, _ := os.ReadFile(gitcfg); return strings.Contains(string(cfg), "insteadOf") }
	waitRule := func(on bool) {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if ruleOn() == on {
				return
			}
		}
		t.Fatalf("the agent didn't turn the rule on=%v", on)
	}

	authorise := exec.Command(x.bin, "client", "authorise")
	authorise.Env = devEnv
	authOut, err := authorise.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := authorise.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(authOut)
	if !lines.Scan() || !strings.HasPrefix(lines.Text(), "Open ") {
		t.Fatalf("client authorise: %q\nserver log:\n%s", lines.Text(), srvLog)
	}
	f := strings.Fields(lines.Text())
	x.hub.ApproveDevice(f[len(f)-1], "ben-gh")
	if !lines.Scan() || lines.Text() != "ok: GitCove will post your work from outages as ben-gh" || authorise.Wait() != nil {
		t.Fatalf("client authorise: %q\nserver log:\n%s", lines.Text(), srvLog)
	}

	x.hub.SetFaults(fakehub.Faults{APIDown: true, GitDown: true})
	if out, e, err := x.cove("failover", "start"); err != nil {
		t.Fatalf("failover start: %v\n%s%s", err, out, e)
	}
	waitRule(true)
	clone := filepath.Join(t.TempDir(), "app")
	run("", "git", "clone", "-q", "https://github.com/"+x.hub.Org+"/app.git", clone)
	if err := os.WriteFile(filepath.Join(clone, "fix.txt"), []byte("fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(clone, "git", "add", "fix.txt")
	run(clone, "git", "commit", "-q", "-m", "fix")
	sha := run(clone, "git", "rev-parse", "HEAD")
	run(clone, "git", "push", "-q", "origin", "HEAD:refs/heads/fix", "HEAD:refs/heads/main")

	req, _ := http.NewRequest("POST", "http://"+x.addr+"/"+x.hub.Org+"/app/issues",
		strings.NewReader(url.Values{"title": {"outage bug"}, "body": {"found during the outage"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("x", tok)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("new issue in the web interface: %v %v", resp, err)
	}
	resp.Body.Close()

	// GitCove goes away and comes back: the agent leaves the rule as it was meanwhile.
	if err := srv.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	time.Sleep(500 * time.Millisecond)
	if !ruleOn() {
		t.Fatal("the agent turned the rule off while GitCove was unreachable")
	}
	srvLog, _, _ = x.serve(t)
	x.waitStatus(t, srvLog, "server back", func(s status) bool { return s.Mode == "failover" })

	x.hub.SetFaults(fakehub.Faults{})
	out, e, err := x.cove("failover", "end")
	if err != nil || !strings.Contains(out, "push to fix") || !strings.Contains(out, "open PR into main") || !strings.Contains(out, "replay approve") ||
		!strings.Contains(out, `new issue "outage bug"`) || !strings.Contains(out, "ben-gh") {
		t.Fatalf("failover end: %v\n%s%s", err, out, e)
	}
	waitRule(false)
	if out, e, err := x.cove("replay", "approve"); err != nil {
		t.Fatalf("replay approve: %v\n%s%s", err, out, e)
	}
	x.waitStatus(t, srvLog, "replay done", func(s status) bool { return s.Mode == "normal" })
	prs := x.hub.PRs("app")
	if refs := x.hub.Refs(t, "app"); refs["refs/heads/fix"] != sha || len(prs) != 1 || prs[0].Base != "main" || refs["refs/heads/"+prs[0].Head] != sha {
		t.Fatalf("GitHub after replay: fix=%s PRs=%+v", refs["refs/heads/fix"], prs)
	}
	var issue *fakehub.Issue
	for _, i := range x.hub.Issues("app") {
		if !i.PR {
			issue = &i
		}
	}
	if issue == nil || issue.Title != "outage bug" || issue.Author != "ben-gh" || !strings.HasPrefix(issue.Body, "found during the outage\n\n<sub>Written on GitCove at ") {
		t.Fatalf("issue on GitHub after replay: %+v", issue)
	}
}

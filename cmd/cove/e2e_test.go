package main

// End to end: the real cove binary (sync, token CLI, serve) against fakehub.

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tabaak/gitcove/fakehub"
)

func TestBinaryEndToEnd(t *testing.T) {
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	env := append(os.Environ(), "COVE_ORG="+hub.Org, "COVE_APP_ID="+hub.AppID, "COVE_APP_KEY_FILE="+keyFile,
		"COVE_DATA="+filepath.Join(dir, "data"), "COVE_LISTEN="+addr, "COVE_GITHUB_API="+hub.URL, "COVE_GITHUB_GIT="+hub.URL,
		"COVE_HEALTH_INTERVAL=200ms", "COVE_SYNC_INTERVAL=1h", "GIT_TERMINAL_PROMPT=0")
	cove := func(args ...string) (string, string, error) {
		var o, e bytes.Buffer
		c := exec.Command(bin, args...)
		c.Env, c.Stdout, c.Stderr = env, &o, &e
		err := c.Run()
		return strings.TrimSpace(o.String()), e.String(), err
	}

	if _, e, err := cove("sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, e)
	}
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

	srv := exec.Command(bin, "serve")
	srv.Env = env
	var srvLog bytes.Buffer
	srv.Stderr = &srvLog
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	t.Cleanup(func() { srv.Process.Kill() })

	type status struct {
		Repos  int
		GitHub struct{ API, Git struct{ OK bool } }
	}
	waitStatus := func(what string, ok func(status) bool) {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			resp, err := http.Get("http://" + addr + "/status")
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
	clone := func(tok string) error {
		out, err := exec.Command("git", "clone", "-q", "http://x:"+tok+"@"+addr+"/"+hub.Org+"/app.git", t.TempDir()+"/c").CombinedOutput()
		if err != nil {
			t.Logf("clone: %s", out)
		}
		return err
	}

	waitStatus("GitHub up", func(s status) bool { return s.Repos == 1 && s.GitHub.API.OK && s.GitHub.Git.OK })
	if err := clone(tok); err != nil {
		t.Fatalf("clone: %v", err)
	}

	hub.SetFaults(fakehub.Faults{APIDown: true, GitDown: true})
	waitStatus("GitHub down", func(s status) bool { return !s.GitHub.API.OK && !s.GitHub.Git.OK })
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

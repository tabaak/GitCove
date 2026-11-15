package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	f, err := lockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockFile(path); !errors.Is(err, errLocked) {
		t.Fatalf("second lock: %v, want errLocked", err)
	}
	f.Close() // as when the agent exits
	f, err = lockFile(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	f.Close()
}

func TestTaskXML(t *testing.T) {
	exe, account := `C:\Program Files\A&B <"it's">\cove.exe`, `PC\o'brien&co`
	var task struct {
		Trigger   string `xml:"Triggers>LogonTrigger>UserId"`
		Principal string `xml:"Principals>Principal>UserId"`
		Command   string `xml:"Actions>Exec>Command"`
		Args      string `xml:"Actions>Exec>Arguments"`
	}
	d := xml.NewDecoder(strings.NewReader(taskXML(exe, account)))
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil } // UTF-16 only once written
	if err := d.Decode(&task); err != nil {
		t.Fatal(err)
	}
	if task.Trigger != account || task.Principal != account || task.Command != "conhost.exe" ||
		task.Args != `--headless "`+exe+`" client agent` {
		t.Fatalf("got %+v", task)
	}
	if got := utf16LE("<é"); !bytes.Equal(got, []byte{0xFF, 0xFE, '<', 0, 0xE9, 0}) {
		t.Fatalf("utf16LE: % x", got)
	}
}

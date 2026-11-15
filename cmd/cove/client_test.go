package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

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

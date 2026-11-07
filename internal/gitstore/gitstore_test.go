package gitstore

import (
	"path/filepath"
	"testing"
)

// A relative data dir once made http-backend look for repos relative to the git binary.
func TestRootIsAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	s, err := New("repos", "https://github.com/")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(s.Root) || s.GitURL != "https://github.com" {
		t.Fatalf("Root = %q, GitURL = %q", s.Root, s.GitURL)
	}
}

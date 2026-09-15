package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileNameRejectsPathTraversalCharacters(t *testing.T) {
	cases := []string{"../victim", `..\\victim`, "a/b", "a:b", "../../../../etc/passwd"}
	for _, uid := range cases {
		t.Run(uid, func(t *testing.T) {
			a := New(uid, "name", "token")
			name := a.FileName()
			if strings.Contains(name, "/") || strings.Contains(name, `\\`) || strings.Contains(name, "..") {
				t.Fatalf("unsafe filename %q from uid %q", name, uid)
			}
			if filepath.Base(name) != name {
				t.Fatalf("filename escaped directory: %q", name)
			}
		})
	}
}

func TestSaveNewRejectsMissingIdentityOrToken(t *testing.T) {
	dir := t.TempDir()
	if err := SaveNew(dir, New("", "name", "token")); err == nil {
		t.Fatal("missing uid must be rejected")
	}
	if err := SaveNew(dir, New("u1", "name", "")); err == nil {
		t.Fatal("missing token must be rejected")
	}
}

func TestLoadDirRejectsDuplicateUID(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "catpaw-one.json"), []byte(`{"uid":"same","access_token":"tok-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catpaw-two.json"), []byte(`{"uid":"same","access_token":"tok-2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "duplicate uid") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadDirRejectsTokenWithoutUID(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "catpaw-bad.json"), []byte(`{"access_token":"tok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "missing uid") {
		t.Fatalf("err=%v", err)
	}
}

package main

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeName(t *testing.T) {
	ok := []string{"install.wim", "default.json", "boot.wim", "a b.wim"}
	bad := []string{"", ".", "..", "a/b", `a\b`, "../x", "x/..", "sub/dir"}
	for _, n := range ok {
		if _, valid := safeName(n); !valid {
			t.Errorf("safeName(%q) = invalid, want valid", n)
		}
	}
	for _, n := range bad {
		if _, valid := safeName(n); valid {
			t.Errorf("safeName(%q) = valid, want invalid", n)
		}
	}
}

func TestResolveRejectsUnknownCategory(t *testing.T) {
	if _, err := resolve(t.TempDir(), "secrets", "x.json"); err == nil {
		t.Fatal("expected error for unknown category")
	}
}

// end-to-end: upload -> list -> delete through the fiber app.
func TestUploadListDelete(t *testing.T) {
	data := t.TempDir()
	app := newApp(data, t.TempDir())

	// upload
	body := strings.NewReader("MSWIM-fake-bytes")
	req := httptest.NewRequest("PUT", "/api/files/images/install.wim", body)
	resp, err := app.Test(req, -1)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("upload: status=%d err=%v", resp.StatusCode, err)
	}
	if _, err := os.Stat(filepath.Join(data, "images", "install.wim")); err != nil {
		t.Fatalf("file not written: %v", err)
	}

	// list
	req = httptest.NewRequest("GET", "/api/files?category=images", nil)
	resp, _ = app.Test(req, -1)
	b, _ := io.ReadAll(resp.Body)
	var files []fileInfo
	if err := json.Unmarshal(b, &files); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(files) != 1 || files[0].Name != "install.wim" {
		t.Fatalf("list = %+v, want one install.wim", files)
	}

	// delete
	req = httptest.NewRequest("DELETE", "/api/files/images/install.wim", nil)
	resp, _ = app.Test(req, -1)
	if resp.StatusCode != 204 {
		t.Fatalf("delete: status=%d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(data, "images", "install.wim")); !os.IsNotExist(err) {
		t.Fatal("file still present after delete")
	}
}

func TestUploadRejectsTraversal(t *testing.T) {
	app := newApp(t.TempDir(), t.TempDir())
	req := httptest.NewRequest("PUT", "/api/files/images/..%2f..%2fetc%2fpasswd", strings.NewReader("x"))
	resp, _ := app.Test(req, -1)
	if resp.StatusCode < 400 {
		t.Fatalf("traversal upload allowed: status=%d", resp.StatusCode)
	}
}

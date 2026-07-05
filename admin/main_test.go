package main

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolvePath(t *testing.T) {
	root := t.TempDir()
	// unknown category rejected
	if _, err := resolvePath(root, "secrets", "x.json"); err == nil {
		t.Fatal("expected error for unknown category")
	}
	// traversal is neutralized (stays under the category root)
	got, err := resolvePath(root, "images", "../../etc/passwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	base := filepath.Join(root, "images")
	if got != base && !strings.HasPrefix(got, base+string(os.PathSeparator)) {
		t.Fatalf("resolvePath escaped root: %q", got)
	}
	// nested path allowed
	if _, err := resolvePath(root, "images", "win11/23h2/install.wim"); err != nil {
		t.Fatalf("nested path rejected: %v", err)
	}
}

// end-to-end: mkdir -> upload into it -> list -> download -> delete folder.
func TestFolderLifecycle(t *testing.T) {
	data := t.TempDir()
	app := newApp(data, t.TempDir())

	// create folder
	if r, _ := app.Test(httptest.NewRequest("POST", "/api/folders/images/win11", nil), -1); r.StatusCode != 201 {
		t.Fatalf("mkdir: %d", r.StatusCode)
	}
	if fi, err := os.Stat(filepath.Join(data, "images", "win11")); err != nil || !fi.IsDir() {
		t.Fatalf("folder not created: %v", err)
	}

	// upload into the folder
	r, _ := app.Test(httptest.NewRequest("PUT", "/api/files/images/win11/install.wim", strings.NewReader("MSWIM")), -1)
	if r.StatusCode != 201 {
		t.Fatalf("upload: %d", r.StatusCode)
	}

	// list the folder -> one file
	r, _ = app.Test(httptest.NewRequest("GET", "/api/files?category=images&prefix=win11", nil), -1)
	b, _ := io.ReadAll(r.Body)
	var files []fileInfo
	if err := json.Unmarshal(b, &files); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(files) != 1 || files[0].Name != "install.wim" || files[0].IsDir {
		t.Fatalf("list = %+v", files)
	}

	// list the root -> the folder shows as isDir
	r, _ = app.Test(httptest.NewRequest("GET", "/api/files?category=images", nil), -1)
	b, _ = io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &files)
	if len(files) != 1 || !files[0].IsDir || files[0].Name != "win11" {
		t.Fatalf("root list = %+v", files)
	}

	// delete the folder recursively
	r, _ = app.Test(httptest.NewRequest("DELETE", "/api/files/images/win11", nil), -1)
	if r.StatusCode != 204 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(data, "images", "win11")); !os.IsNotExist(err) {
		t.Fatal("folder still present after delete")
	}
}

func TestDownload(t *testing.T) {
	// fasthttp's file server caches the open fd ~10s; on Windows that blocks the
	// t.TempDir cleanup. The download path is exercised on Linux (CI runtime).
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows: fasthttp holds the file handle, blocking temp cleanup")
	}
	data := t.TempDir()
	app := newApp(data, t.TempDir())
	app.Test(httptest.NewRequest("PUT", "/api/files/config/default.json", strings.NewReader(`{"ok":1}`)), -1)

	r, _ := app.Test(httptest.NewRequest("GET", "/api/download/config/default.json", nil), -1)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || string(b) != `{"ok":1}` {
		t.Fatalf("download: %d %q", r.StatusCode, string(b))
	}
	// a folder is not downloadable
	app.Test(httptest.NewRequest("POST", "/api/folders/config/sub", nil), -1)
	if r, _ := app.Test(httptest.NewRequest("GET", "/api/download/config/sub", nil), -1); r.StatusCode != 404 {
		t.Fatalf("expected 404 downloading a folder, got %d", r.StatusCode)
	}
}

func TestRejectsCategoryRootDelete(t *testing.T) {
	app := newApp(t.TempDir(), t.TempDir())
	r, _ := app.Test(httptest.NewRequest("DELETE", "/api/files/images/", nil), -1)
	if r.StatusCode < 400 {
		t.Fatalf("root delete allowed: %d", r.StatusCode)
	}
}

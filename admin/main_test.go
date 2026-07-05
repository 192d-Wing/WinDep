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

	"github.com/gofiber/fiber/v2"
)

func testApp(t *testing.T, data string) *fiber.App {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { st.close() })
	return newApp(data, t.TempDir(), st)
}

func TestResolvePath(t *testing.T) {
	root := t.TempDir()
	if _, err := resolvePath(root, "secrets", "x.json"); err == nil {
		t.Fatal("expected error for unknown category")
	}
	got, err := resolvePath(root, "images", "../../etc/passwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	base := filepath.Join(root, "images")
	if got != base && !strings.HasPrefix(got, base+string(os.PathSeparator)) {
		t.Fatalf("resolvePath escaped root: %q", got)
	}
	if _, err := resolvePath(root, "images", "win11/23h2/install.wim"); err != nil {
		t.Fatalf("nested path rejected: %v", err)
	}
}

// end-to-end: mkdir -> upload into it -> list -> delete folder.
func TestFolderLifecycle(t *testing.T) {
	data := t.TempDir()
	app := testApp(t, data)

	if r, _ := app.Test(httptest.NewRequest("POST", "/api/folders/images/win11", nil), -1); r.StatusCode != 201 {
		t.Fatalf("mkdir: %d", r.StatusCode)
	}
	if fi, err := os.Stat(filepath.Join(data, "images", "win11")); err != nil || !fi.IsDir() {
		t.Fatalf("folder not created: %v", err)
	}
	r, _ := app.Test(httptest.NewRequest("PUT", "/api/files/images/win11/install.wim", strings.NewReader("MSWIM")), -1)
	if r.StatusCode != 201 {
		t.Fatalf("upload: %d", r.StatusCode)
	}
	r, _ = app.Test(httptest.NewRequest("GET", "/api/files?category=images&prefix=win11", nil), -1)
	b, _ := io.ReadAll(r.Body)
	var files []fileInfo
	if err := json.Unmarshal(b, &files); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(files) != 1 || files[0].Name != "install.wim" || files[0].IsDir {
		t.Fatalf("list = %+v", files)
	}
	r, _ = app.Test(httptest.NewRequest("GET", "/api/files?category=images", nil), -1)
	b, _ = io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &files)
	if len(files) != 1 || !files[0].IsDir || files[0].Name != "win11" {
		t.Fatalf("root list = %+v", files)
	}
	r, _ = app.Test(httptest.NewRequest("DELETE", "/api/files/images/win11", nil), -1)
	if r.StatusCode != 204 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(data, "images", "win11")); !os.IsNotExist(err) {
		t.Fatal("folder still present after delete")
	}
}

func TestDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows: fasthttp holds the file handle, blocking temp cleanup")
	}
	data := t.TempDir()
	app := testApp(t, data)
	app.Test(httptest.NewRequest("PUT", "/api/files/config/default.json", strings.NewReader(`{"ok":1}`)), -1)

	r, _ := app.Test(httptest.NewRequest("GET", "/api/download/config/default.json", nil), -1)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || string(b) != `{"ok":1}` {
		t.Fatalf("download: %d %q", r.StatusCode, string(b))
	}
	app.Test(httptest.NewRequest("POST", "/api/folders/config/sub", nil), -1)
	if r, _ := app.Test(httptest.NewRequest("GET", "/api/download/config/sub", nil), -1); r.StatusCode != 404 {
		t.Fatalf("expected 404 downloading a folder, got %d", r.StatusCode)
	}
}

func TestRejectsCategoryRootDelete(t *testing.T) {
	app := testApp(t, t.TempDir())
	r, _ := app.Test(httptest.NewRequest("DELETE", "/api/files/images/", nil), -1)
	if r.StatusCode < 400 {
		t.Fatalf("root delete allowed: %d", r.StatusCode)
	}
}

// ingest a status/log, then read it back via the review endpoint.
func TestIngestAndLogs(t *testing.T) {
	app := testApp(t, t.TempDir())

	app.Test(httptest.NewRequest("POST", "/api/ingest/status",
		strings.NewReader(`{"serial":"5CG1","state":"progress","percent":62,"message":"Applying image","model":"OptiPlex"}`)), -1)
	app.Test(httptest.NewRequest("POST", "/api/ingest/log",
		strings.NewReader(`{"serial":"5CG1","lines":[{"level":"info","message":"partitioned"}]}`)), -1)

	r, _ := app.Test(httptest.NewRequest("GET", "/api/logs?serial=5CG1", nil), -1)
	b, _ := io.ReadAll(r.Body)
	var events []map[string]any
	if err := json.Unmarshal(b, &events); err != nil {
		t.Fatalf("logs decode: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d: %v", len(events), events)
	}
}

// file operations should leave an audit trail (writes and reads).
func TestAuditTrail(t *testing.T) {
	app := testApp(t, t.TempDir())
	app.Test(httptest.NewRequest("POST", "/api/folders/config/x", nil), -1)
	app.Test(httptest.NewRequest("PUT", "/api/files/config/x/a.json", strings.NewReader("{}")), -1)
	app.Test(httptest.NewRequest("GET", "/api/files?category=config&prefix=x", nil), -1)

	r, _ := app.Test(httptest.NewRequest("GET", "/api/audit", nil), -1)
	b, _ := io.ReadAll(r.Body)
	var entries []map[string]any
	if err := json.Unmarshal(b, &entries); err != nil {
		t.Fatalf("audit decode: %v", err)
	}
	// mkdir + upload + list = 3 audited ops (the /api/audit read itself is not audited)
	if len(entries) < 3 {
		t.Fatalf("want >=3 audit entries, got %d: %v", len(entries), entries)
	}
	actions := map[string]bool{}
	for _, e := range entries {
		actions[e["action"].(string)] = true
	}
	for _, want := range []string{"mkdir", "upload", "list"} {
		if !actions[want] {
			t.Errorf("missing audit action %q", want)
		}
	}
}

// a failed op must be audited with its real status, not the default 200.
func TestAuditRecordsFailureStatus(t *testing.T) {
	app := testApp(t, t.TempDir())
	r, _ := app.Test(httptest.NewRequest("DELETE", "/api/files/images/nope.wim", nil), -1)
	if r.StatusCode != 404 {
		t.Fatalf("want 404 deleting missing file, got %d", r.StatusCode)
	}
	r, _ = app.Test(httptest.NewRequest("GET", "/api/audit", nil), -1)
	b, _ := io.ReadAll(r.Body)
	var entries []map[string]any
	_ = json.Unmarshal(b, &entries)
	found := false
	for _, e := range entries {
		if e["action"] == "delete" {
			found = true
			if got := int(e["status"].(float64)); got != 404 {
				t.Fatalf("audit status = %d, want 404 (regression: failures logged as 200)", got)
			}
		}
	}
	if !found {
		t.Fatal("no delete audit entry recorded")
	}
}

// fleet returns the latest status per machine.
func TestFleet(t *testing.T) {
	app := testApp(t, t.TempDir())
	// two statuses for the same serial (progress -> success) + one other machine
	for _, body := range []string{
		`{"serial":"AAA","state":"progress","percent":40}`,
		`{"serial":"AAA","state":"success","percent":100}`,
		`{"serial":"BBB","state":"progress","percent":10}`,
	} {
		app.Test(httptest.NewRequest("POST", "/api/ingest/status", strings.NewReader(body)), -1)
	}
	r, _ := app.Test(httptest.NewRequest("GET", "/api/fleet", nil), -1)
	b, _ := io.ReadAll(r.Body)
	var fleet []map[string]any
	_ = json.Unmarshal(b, &fleet)
	if len(fleet) != 2 {
		t.Fatalf("want 2 machines, got %d: %v", len(fleet), fleet)
	}
	byS := map[string]map[string]any{}
	for _, m := range fleet {
		byS[m["serial"].(string)] = m
	}
	if byS["AAA"]["state"] != "success" || byS["AAA"]["percent"].(float64) != 100 {
		t.Fatalf("AAA latest = %v, want success/100", byS["AAA"])
	}
}

func TestPrune(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.close()
	for i := 0; i < 10; i++ {
		if err := st.addAudit("list", "images", "", "127.0.0.1", 0, 200); err != nil {
			t.Fatalf("addAudit: %v", err)
		}
	}
	if err := st.prune(3); err != nil {
		t.Fatalf("prune: %v", err)
	}
	entries, err := st.auditEntries(100)
	if err != nil {
		t.Fatalf("auditEntries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 rows after prune(3), got %d", len(entries))
	}
}

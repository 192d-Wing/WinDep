package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// buildWIM writes a minimal but structurally-valid WIM to path: the 208-byte header
// with a valid magic and an rhXmlData resource pointing at an appended UTF-16LE XML
// blob. Enough for wimImages to exercise the real header/XML parse path.
func buildWIM(t *testing.T, path, xml string) {
	t.Helper()
	u16 := []byte{0xFF, 0xFE} // UTF-16LE BOM
	for _, r := range xml {
		u16 = append(u16, byte(r), byte(r>>8))
	}
	hdr := make([]byte, wimHeaderLen)
	copy(hdr, wimMagic)
	rh := hdr[xmlReshdrOff:]
	binary.LittleEndian.PutUint64(rh[0:8], uint64(len(u16))) // size, flags=0 (uncompressed)
	binary.LittleEndian.PutUint64(rh[8:16], wimHeaderLen)    // offset = right after header
	binary.LittleEndian.PutUint64(rh[16:24], uint64(len(u16)))
	if err := os.WriteFile(path, append(hdr, u16...), 0o644); err != nil {
		t.Fatalf("write wim: %v", err)
	}
}

func TestWIMImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.wim")
	buildWIM(t, path, `<WIM><IMAGE INDEX="1"><NAME>Windows 11 Pro</NAME>`+
		`<WINDOWS><ARCH>9</ARCH><EDITIONID>Professional</EDITIONID>`+
		`<VERSION><MAJOR>10</MAJOR><MINOR>0</MINOR><BUILD>22631</BUILD><SPBUILD>2861</SPBUILD></VERSION>`+
		`</WINDOWS><TOTALBYTES>1234</TOTALBYTES></IMAGE></WIM>`)

	imgs, err := wimImages(path)
	if err != nil {
		t.Fatalf("wimImages: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d: %+v", len(imgs), imgs)
	}
	got := imgs[0]
	want := WIMImage{Index: 1, Name: "Windows 11 Pro", Edition: "Professional", Arch: "x64", Build: "10.0.22631.2861", Size: 1234}
	if got != want {
		t.Fatalf("image = %+v, want %+v", got, want)
	}

	// A non-WIM file is rejected, not parsed as an empty catalogue.
	notWim := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(notWim, []byte("hello, not a wim"), 0o644)
	if _, err := wimImages(notWim); err == nil {
		t.Fatal("expected error parsing non-WIM file")
	}
}

// upload records a SHA-256 sidecar, hidden from the listing but surfaced as fileInfo.Sha256.
func TestUploadRecordsChecksum(t *testing.T) {
	data := t.TempDir()
	app := testApp(t, data)
	body := "install-image-bytes"
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))

	if r, _ := app.Test(httptest.NewRequest("PUT", "/api/files/images/a.wim", strings.NewReader(body)), -1); r.StatusCode != 201 {
		t.Fatalf("upload: %d", r.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(data, "images", "a.wim.sha256")); err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}

	r, _ := app.Test(httptest.NewRequest("GET", "/api/files?category=images", nil), -1)
	b, _ := io.ReadAll(r.Body)
	var files []fileInfo
	if err := json.Unmarshal(b, &files); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(files) != 1 { // the .sha256 sidecar is hidden
		t.Fatalf("want 1 listed file (sidecar hidden), got %d: %+v", len(files), files)
	}
	if files[0].Sha256 != sum {
		t.Fatalf("sha256 = %q, want %q", files[0].Sha256, sum)
	}
}

// verify re-hashes on disk and flags tampering against the recorded checksum.
func TestVerify(t *testing.T) {
	data := t.TempDir()
	app := testApp(t, data)
	app.Test(httptest.NewRequest("PUT", "/api/files/images/a.wim", strings.NewReader("good-bytes")), -1)

	verify := func() map[string]any {
		r, _ := app.Test(httptest.NewRequest("POST", "/api/verify/images/a.wim", nil), -1)
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("verify decode (%d): %v", r.StatusCode, err)
		}
		return m
	}

	if m := verify(); m["ok"] != true {
		t.Fatalf("clean file should verify: %v", m)
	}
	// Tamper with the file on disk without updating the sidecar.
	if err := os.WriteFile(filepath.Join(data, "images", "a.wim"), []byte("EVIL-bytes"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if m := verify(); m["ok"] != false {
		t.Fatalf("tampered file should fail verify: %v", m)
	}

	// No sidecar -> 404 (nothing to verify against).
	r, _ := app.Test(httptest.NewRequest("POST", "/api/verify/config/none.json", nil), -1)
	if r.StatusCode != 404 {
		t.Fatalf("verify with no checksum: want 404, got %d", r.StatusCode)
	}
}

// patchChunk sends a resumable PATCH with the offset/length headers and returns the
// response, so tests can assert status + the server's authoritative Upload-Offset.
func patchChunk(t *testing.T, app *fiber.App, url string, offset, total int, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("PATCH", url, strings.NewReader(body))
	req.Header.Set("Upload-Offset", fmt.Sprint(offset))
	req.Header.Set("Upload-Length", fmt.Sprint(total))
	r, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	return r
}

// a dropped upload resumes from the server's staged offset instead of restarting.
func TestResumableUpload(t *testing.T) {
	data := t.TempDir()
	app := testApp(t, data)
	url := "/api/files/images/big.wim"
	part1, part2 := "the-first-half-of-the-image", "...and-the-second-half!!"
	full := part1 + part2
	total := len(full)
	wantSum := fmt.Sprintf("%x", sha256.Sum256([]byte(full)))

	// HEAD a never-seen target: offset 0, not complete.
	r, _ := app.Test(httptest.NewRequest("HEAD", url, nil), -1)
	if r.Header.Get("Upload-Offset") != "0" || r.Header.Get("Upload-Complete") != "0" {
		t.Fatalf("initial HEAD: offset=%q complete=%q", r.Header.Get("Upload-Offset"), r.Header.Get("Upload-Complete"))
	}

	// First chunk: staged, not finalized (204), offset advances.
	r = patchChunk(t, app, url, 0, total, part1)
	if r.StatusCode != 204 || r.Header.Get("Upload-Offset") != fmt.Sprint(len(part1)) {
		t.Fatalf("chunk1: status=%d offset=%q", r.StatusCode, r.Header.Get("Upload-Offset"))
	}
	if _, err := os.Stat(filepath.Join(data, "images", "big.wim")); !os.IsNotExist(err) {
		t.Fatal("final file should not exist until finalize")
	}

	// A stale/duplicate chunk at the wrong offset is rejected with the true offset.
	r = patchChunk(t, app, url, 0, total, part1)
	if r.StatusCode != 409 || r.Header.Get("Upload-Offset") != fmt.Sprint(len(part1)) {
		t.Fatalf("offset mismatch: want 409 + offset %d, got %d + %q", len(part1), r.StatusCode, r.Header.Get("Upload-Offset"))
	}

	// HEAD mid-upload reports the staged offset so the client can resume.
	r, _ = app.Test(httptest.NewRequest("HEAD", url, nil), -1)
	if r.Header.Get("Upload-Offset") != fmt.Sprint(len(part1)) || r.Header.Get("Upload-Complete") != "0" {
		t.Fatalf("mid HEAD: offset=%q complete=%q", r.Header.Get("Upload-Offset"), r.Header.Get("Upload-Complete"))
	}

	// Final chunk finalizes: 200, complete, correct bytes + checksum sidecar.
	r = patchChunk(t, app, url, len(part1), total, part2)
	if r.StatusCode != 200 || r.Header.Get("Upload-Complete") != "1" {
		t.Fatalf("chunk2: status=%d complete=%q", r.StatusCode, r.Header.Get("Upload-Complete"))
	}
	got, err := os.ReadFile(filepath.Join(data, "images", "big.wim"))
	if err != nil || string(got) != full {
		t.Fatalf("assembled file = %q (err %v), want %q", string(got), err, full)
	}
	if sc := readSidecar(filepath.Join(data, "images", "big.wim")); sc != wantSum {
		t.Fatalf("sidecar sha256 = %q, want %q", sc, wantSum)
	}

	// HEAD a finished upload: full size, complete.
	r, _ = app.Test(httptest.NewRequest("HEAD", url, nil), -1)
	if r.Header.Get("Upload-Offset") != fmt.Sprint(total) || r.Header.Get("Upload-Complete") != "1" {
		t.Fatalf("final HEAD: offset=%q complete=%q", r.Header.Get("Upload-Offset"), r.Header.Get("Upload-Complete"))
	}

	// The multi-chunk upload is audited exactly once (the finalize), not per chunk.
	ar, _ := app.Test(httptest.NewRequest("GET", "/api/audit", nil), -1)
	b, _ := io.ReadAll(ar.Body)
	var entries []map[string]any
	_ = json.Unmarshal(b, &entries)
	uploads := 0
	for _, e := range entries {
		if e["action"] == "upload" {
			uploads++
			if int(e["size"].(float64)) != total {
				t.Errorf("audit size = %v, want %d", e["size"], total)
			}
		}
	}
	if uploads != 1 {
		t.Fatalf("want exactly 1 upload audit entry (finalize only), got %d", uploads)
	}
}

// deleting an interrupted upload discards its .part staging file.
func TestDeleteAbortsPartial(t *testing.T) {
	data := t.TempDir()
	app := testApp(t, data)
	url := "/api/files/images/aborted.wim"

	if r := patchChunk(t, app, url, 0, 100, "only-a-partial-chunk"); r.StatusCode != 204 {
		t.Fatalf("partial chunk: %d", r.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(data, "images", "aborted.wim.part")); err != nil {
		t.Fatalf(".part should exist mid-upload: %v", err)
	}
	r, _ := app.Test(httptest.NewRequest("DELETE", url, nil), -1)
	if r.StatusCode != 204 {
		t.Fatalf("abort delete: %d", r.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(data, "images", "aborted.wim.part")); !os.IsNotExist(err) {
		t.Fatal(".part should be gone after abort")
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

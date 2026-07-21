package main

import (
	"database/sql"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

// TestIngestAppRealListener drives newIngestApp over a REAL TCP listener + http.Client
// (not app.Test()), matching the cluster request path where readBody reads the streamed
// body. Reproduces the ingest 500 seen in live validation if the body handling panics.
func TestIngestAppRealListener(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "real.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	app := newIngestApp(st)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.Shutdown() }()
	time.Sleep(150 * time.Millisecond) // let the server come up

	resp, err := http.Post("http://"+ln.Addr().String()+"/api/ingest/status",
		"application/json", strings.NewReader(`{"serial":"RT-1","state":"success","percent":100}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("real-listener ingest: got %d (%s), want 202", resp.StatusCode, string(body))
	}
}

// TestIngestAppAcceptsStatus drives newIngestApp end-to-end (readBody uses
// RequestBodyStream, which panics unless the app sets StreamRequestBody) and asserts a
// status POST is Accepted, not a bare 500.
func TestIngestAppAcceptsStatus(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "ingest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	app := newIngestApp(st)
	for _, tc := range []struct{ path, body string }{
		{"/api/ingest/status", `{"serial":"T-1","state":"success","percent":100}`},
		{"/api/ingest/log", `{"serial":"T-2","lines":[{"ts":"t","level":"info","message":"hi"}]}`},
	} {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if resp.StatusCode != fiber.StatusAccepted {
			t.Fatalf("%s: got %d, want %d", tc.path, resp.StatusCode, fiber.StatusAccepted)
		}
	}
}

// TestMigrateDeployEventNotNullDrift reproduces a legacy deploy_event table with stray
// NOT NULL constraints on columns that are nullable in the current schema (the drift
// found on a preserved DB during live validation) and asserts the migration rebuilds it
// so ingest inserts succeed and existing rows are preserved.
func TestMigrateDeployEventNotNullDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drift.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// state + level marked NOT NULL — addStatus omits level, addLog omits state, so BOTH
	// ingest paths would fail against this table.
	if _, err := raw.Exec(`CREATE TABLE deploy_event(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts TEXT NOT NULL, kind TEXT NOT NULL,
		serial TEXT, mac TEXT,
		state TEXT NOT NULL, percent INTEGER, message TEXT, model TEXT, level TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO deploy_event(ts,kind,serial,state,level)
		VALUES('2026-01-01T00:00:00Z','status','OLD-1','success','info')`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	st, err := openStore(path) // runs migrateDeployEvent
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.close()

	if err := st.addStatus(StatusReport{Serial: "NEW-1", State: "progress", Percent: 50}); err != nil {
		t.Fatalf("addStatus after migration (omits level): %v", err)
	}
	if err := st.addLog("NEW-2", "aa:bb", "info", "hello", "2026-01-02T00:00:00Z"); err != nil {
		t.Fatalf("addLog after migration (omits state/percent/model): %v", err)
	}

	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM deploy_event WHERE serial='OLD-1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("legacy row not preserved through rebuild: got %d, want 1", n)
	}
}

// TestMigrateDeployEventFresh confirms a fresh canonical DB migrates as a no-op and
// accepts ingest writes.
func TestMigrateDeployEventFresh(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	if err := st.addStatus(StatusReport{Serial: "X", State: "success", Percent: 100}); err != nil {
		t.Fatalf("addStatus on fresh DB: %v", err)
	}
}

package main

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO — fits the FIPS static build)
)

// Store is the admin's lightweight datastore: deployment telemetry (forwarded by
// windep-api) and the file-operation audit trail. It is owned by the single-replica
// admin pod on a dedicated RWO volume, so SQLite's single-writer model is safe.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS deploy_event (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  ts      TEXT NOT NULL,   -- server receive time, RFC3339 UTC
  kind    TEXT NOT NULL,   -- 'status' | 'log'
  serial  TEXT, mac TEXT,
  state   TEXT, percent INTEGER, message TEXT, model TEXT,
  level   TEXT
);
CREATE INDEX IF NOT EXISTS idx_de_serial ON deploy_event(serial);

CREATE TABLE IF NOT EXISTS audit (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts       TEXT NOT NULL,
  action   TEXT NOT NULL,  -- upload|delete|mkdir|download|list|verify
  category TEXT, path TEXT,
  source   TEXT,           -- client IP (no user auth yet; NetworkPolicy is the control)
  size     INTEGER,
  status   INTEGER         -- HTTP status of the operation
);`

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// WAL lets readers run concurrently with the single writer, so allow a small
	// pool (UI queries no longer block behind ingest inserts); busy_timeout retries
	// the rare writer-vs-writer contention.
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// prune caps each table to its newest maxRows, bounding growth on the fixed volume.
func (s *Store) prune(maxRows int) error {
	for _, tbl := range []string{"deploy_event", "audit"} {
		if _, err := s.db.Exec(
			`DELETE FROM `+tbl+` WHERE id NOT IN (SELECT id FROM `+tbl+` ORDER BY id DESC LIMIT ?)`,
			maxRows); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) close() error { return s.db.Close() }

func (s *Store) addStatus(r StatusReport) error {
	_, err := s.db.Exec(
		`INSERT INTO deploy_event(ts,kind,serial,mac,state,percent,message,model) VALUES(?,?,?,?,?,?,?,?)`,
		nowUTC(), "status", r.Serial, r.Mac, r.State, r.Percent, r.Message, r.Model)
	return err
}

// addLog stores a WinPE log line. ts is the device-provided event time; it falls
// back to server-receive time only when the agent didn't supply one, so ordering
// reflects when the event happened on the machine, not when the batch was processed.
func (s *Store) addLog(serial, mac, level, message, ts string) error {
	if ts == "" {
		ts = nowUTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO deploy_event(ts,kind,serial,mac,level,message) VALUES(?,?,?,?,?,?)`,
		ts, "log", serial, mac, level, message)
	return err
}

func (s *Store) addAudit(action, category, path, source string, size int64, status int) error {
	_, err := s.db.Exec(
		`INSERT INTO audit(ts,action,category,path,source,size,status) VALUES(?,?,?,?,?,?,?)`,
		nowUTC(), action, category, path, source, size, status)
	return err
}

// deployEvents returns the most recent events (optionally filtered by serial),
// newest first, as JSON-ready maps.
func (s *Store) deployEvents(serial string, limit int) ([]map[string]any, error) {
	q := `SELECT ts,kind,serial,mac,state,percent,message,model,level FROM deploy_event`
	args := []any{}
	if serial != "" {
		q += ` WHERE serial=?`
		args = append(args, serial)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ts, kind string
		var serialCol, mac, state, message, model, level sql.NullString
		var percent sql.NullInt64
		if err := rows.Scan(&ts, &kind, &serialCol, &mac, &state, &percent, &message, &model, &level); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"ts": ts, "kind": kind, "serial": serialCol.String, "mac": mac.String,
			"state": state.String, "percent": percent.Int64, "message": message.String,
			"model": model.String, "level": level.String,
		})
	}
	return out, rows.Err()
}

// fleet returns the latest status per machine (serial) — the live fleet view.
// The newest status row per serial is picked by MAX(id) (monotonic insert order),
// which is robust regardless of clock skew in the ts field.
func (s *Store) fleet() ([]map[string]any, error) {
	rows, err := s.db.Query(`
		SELECT serial, state, percent, message, model, ts FROM deploy_event
		WHERE kind='status' AND id IN (
			SELECT MAX(id) FROM deploy_event WHERE kind='status' AND serial IS NOT NULL GROUP BY serial
		)
		ORDER BY ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var serial, state, message, model, ts sql.NullString
		var percent sql.NullInt64
		if err := rows.Scan(&serial, &state, &percent, &message, &model, &ts); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"serial": serial.String, "state": state.String, "percent": percent.Int64,
			"message": message.String, "model": model.String, "ts": ts.String,
		})
	}
	return out, rows.Err()
}

func (s *Store) auditEntries(limit int) ([]map[string]any, error) {
	rows, err := s.db.Query(
		`SELECT ts,action,category,path,source,size,status FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ts, action string
		var category, path, source sql.NullString
		var size, status sql.NullInt64
		if err := rows.Scan(&ts, &action, &category, &path, &source, &size, &status); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"ts": ts, "action": action, "category": category.String, "path": path.String,
			"source": source.String, "size": size.Int64, "status": status.Int64,
		})
	}
	return out, rows.Err()
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

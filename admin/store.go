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
CREATE INDEX IF NOT EXISTS idx_de_id     ON deploy_event(id);

CREATE TABLE IF NOT EXISTS audit (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts       TEXT NOT NULL,
  action   TEXT NOT NULL,  -- upload|delete|mkdir|download|list
  category TEXT, path TEXT,
  source   TEXT,           -- client IP (no user auth yet; NetworkPolicy is the control)
  size     INTEGER,
  status   INTEGER         -- HTTP status of the operation
);
CREATE INDEX IF NOT EXISTS idx_au_id ON audit(id);`

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // serialize access; SQLite is single-writer
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) close() error { return s.db.Close() }

func (s *Store) addStatus(r StatusReport) error {
	_, err := s.db.Exec(
		`INSERT INTO deploy_event(ts,kind,serial,mac,state,percent,message,model) VALUES(?,?,?,?,?,?,?,?)`,
		nowUTC(), "status", r.Serial, r.Mac, r.State, r.Percent, r.Message, r.Model)
	return err
}

func (s *Store) addLog(serial, mac, level, message string) error {
	_, err := s.db.Exec(
		`INSERT INTO deploy_event(ts,kind,serial,mac,level,message) VALUES(?,?,?,?,?,?)`,
		nowUTC(), "log", serial, mac, level, message)
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
		var serial, mac, state, message, model, level sql.NullString
		var percent sql.NullInt64
		if err := rows.Scan(&ts, &kind, &serial, &mac, &state, &percent, &message, &model, &level); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"ts": ts, "kind": kind, "serial": serial.String, "mac": mac.String,
			"state": state.String, "percent": percent.Int64, "message": message.String,
			"model": model.String, "level": level.String,
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

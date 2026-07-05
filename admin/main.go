// Command windep-admin is the read-write admin backend for the WinDep deploy
// server. It exposes an S3-console-style file API over the PersistentVolume
// (browse folders, upload, create folder, download, delete), serves the Cloudscape
// SPA that drives it, and keeps a lightweight SQLite datastore for two review
// surfaces: deployment telemetry (forwarded by windep-api) and a file-operation
// audit trail.
//
// It is the ONLY writer to the deploy PV; the client-facing nginx mounts the same
// volume read-only. Because this is the RW surface, it is meant to sit behind a
// locked-down admin VIP (NetworkPolicy) and, when ADMIN_TOKEN is set, bearer auth.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// categories are the only top-level subtrees of the deploy PV the admin UI may
// touch. Folders may be nested freely beneath each.
var categories = map[string]bool{"images": true, "config": true, "boot": true}

const errQuery = "query failed"

type fileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	IsDir    bool   `json:"isDir"`
}

// StatusReport / LogBatch mirror the payloads windep-api receives from WinPE and
// forwards to /api/ingest.
type StatusReport struct {
	Serial  string `json:"serial"`
	Mac     string `json:"mac"`
	State   string `json:"state"`
	Percent int    `json:"percent"`
	Message string `json:"message"`
	Model   string `json:"model"`
}

type LogLine struct {
	Ts      string `json:"ts"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type LogBatch struct {
	Serial string    `json:"serial"`
	Mac    string    `json:"mac"`
	Lines  []LogLine `json:"lines"`
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func queryInt(c *fiber.Ctx, key string, def, max int) int {
	n, err := strconv.Atoi(c.Query(key))
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// resolvePath maps (category, relative URL path) to an absolute filesystem path
// under DATA_DIR/<category>, neutralizing any "../" traversal.
func resolvePath(dataDir, category, rel string) (string, error) {
	if !categories[category] {
		return "", fiber.NewError(fiber.StatusBadRequest, "unknown category")
	}
	clean := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(rel, "/")), "/")
	root := filepath.Join(dataDir, category)
	full := filepath.Join(root, filepath.FromSlash(clean))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fiber.NewError(fiber.StatusBadRequest, "invalid path")
	}
	return full, nil
}

func handleList(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		category := c.Query("category", "images")
		dir, err := resolvePath(dataDir, category, c.Query("prefix"))
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return c.JSON([]fileInfo{})
			}
			return fiber.NewError(fiber.StatusInternalServerError, "read failed")
		}
		out := make([]fileInfo, 0, len(entries))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			fi := fileInfo{Name: e.Name(), IsDir: e.IsDir(), Modified: info.ModTime().UTC().Format("2006-01-02T15:04:05Z")}
			if !e.IsDir() {
				fi.Size = info.Size()
			}
			out = append(out, fi)
		}
		return c.JSON(out)
	}
}

// syncEvery bounds how much unflushed (dirty) page cache a large upload may
// accumulate before it OOMKills the pod against the container memory cap.
const syncEvery = 128 << 20 // 128 MiB

// writeChunk writes b to dst and fsyncs once *pending crosses syncEvery, resetting it.
func writeChunk(dst *os.File, b []byte, pending *int64) error {
	if _, err := dst.Write(b); err != nil {
		return err
	}
	if *pending += int64(len(b)); *pending >= syncEvery {
		if err := dst.Sync(); err != nil {
			return err
		}
		*pending = 0
	}
	return nil
}

// copySynced streams src -> dst, fsync-ing every syncEvery bytes to keep dirty page
// cache bounded regardless of file size.
func copySynced(dst *os.File, src io.Reader) error {
	buf := make([]byte, 4<<20)
	var pending int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if err := writeChunk(dst, buf[:n], &pending); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return dst.Sync()
		}
		if rerr != nil {
			return rerr
		}
	}
}

// saveStream writes src to a ".part" temp beside dest, then atomically renames it.
func saveStream(dest string, src io.Reader) (err error) {
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if err = copySynced(f, src); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func handleUpload(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		rel := c.Params("*")
		if rel == "" || strings.HasSuffix(rel, "/") {
			return fiber.NewError(fiber.StatusBadRequest, "missing file name")
		}
		dest, err := resolvePath(dataDir, c.Params("category"), rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "mkdir failed")
		}
		if err := saveStream(dest, c.Context().RequestBodyStream()); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "write failed")
		}
		return c.SendStatus(fiber.StatusCreated)
	}
}

func handleMkdir(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		rel := c.Params("*")
		if strings.Trim(rel, "/") == "" {
			return fiber.NewError(fiber.StatusBadRequest, "missing folder name")
		}
		dest, err := resolvePath(dataDir, c.Params("category"), rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "mkdir failed")
		}
		return c.SendStatus(fiber.StatusCreated)
	}
}

func handleDelete(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		rel := c.Params("*")
		if strings.Trim(rel, "/") == "" {
			return fiber.NewError(fiber.StatusBadRequest, "refusing to delete category root")
		}
		dest, err := resolvePath(dataDir, c.Params("category"), rel)
		if err != nil {
			return err
		}
		if _, err := os.Stat(dest); err != nil {
			if os.IsNotExist(err) {
				return fiber.NewError(fiber.StatusNotFound, "not found")
			}
			return fiber.NewError(fiber.StatusInternalServerError, "stat failed")
		}
		if err := os.RemoveAll(dest); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "delete failed")
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// handleDownload streams a file as an attachment. SetBodyStream(*os.File) makes
// fasthttp close the fd as soon as the response is sent (unlike c.Download's caching
// server), which on the NFS volume avoids silly-rename blocking a later folder rmdir.
func handleDownload(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		src, err := resolvePath(dataDir, c.Params("category"), c.Params("*"))
		if err != nil {
			return err
		}
		info, err := os.Stat(src)
		if err != nil || info.IsDir() {
			return fiber.NewError(fiber.StatusNotFound, "not a file")
		}
		f, err := os.Open(src)
		if err != nil {
			return fiber.NewError(fiber.StatusNotFound, "open failed")
		}
		c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+filepath.Base(src)+`"`)
		c.Type("bin")
		c.Context().Response.SetBodyStream(f, int(info.Size()))
		return nil
	}
}

// --- datastore-backed handlers: ingest (from windep-api) + review (for the UI) ---

// readJSON decodes the (stream-mode) request body into v. StreamRequestBody makes
// c.Body()/BodyParser unreliable, so read the stream directly, bounded by a small
// cap so a runaway/hostile ingest body can't buffer gigabytes and OOM the pod.
func readJSON(c *fiber.Ctx, v any) error {
	body, err := io.ReadAll(io.LimitReader(c.Context().RequestBodyStream(), 1<<20)) // 1 MiB
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func handleIngestStatus(st *Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var r StatusReport
		if err := readJSON(c, &r); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid json")
		}
		if err := st.addStatus(r); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "store failed")
		}
		return c.SendStatus(fiber.StatusAccepted)
	}
}

func handleIngestLog(st *Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var b LogBatch
		if err := readJSON(c, &b); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid json")
		}
		for _, l := range b.Lines {
			if err := st.addLog(b.Serial, b.Mac, l.Level, l.Message, l.Ts); err != nil {
				return fiber.NewError(fiber.StatusInternalServerError, "store failed")
			}
		}
		return c.SendStatus(fiber.StatusAccepted)
	}
}

func handleLogs(st *Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		events, err := st.deployEvents(c.Query("serial"), queryInt(c, "limit", 200, 2000))
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, errQuery)
		}
		return c.JSON(events)
	}
}

func handleAudit(st *Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		entries, err := st.auditEntries(queryInt(c, "limit", 200, 2000))
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, errQuery)
		}
		return c.JSON(entries)
	}
}

func handleFleet(st *Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		fleet, err := st.fleet()
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, errQuery)
		}
		return c.JSON(fleet)
	}
}

// auditMW records every file operation (reads and writes) to the audit trail after
// the handler runs, tagging it with the resulting status and the client IP.
func auditMW(st *Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := c.Next()
		action, category, p, size := classify(c)
		if action != "" {
			// On error the ErrorHandler sets the real status AFTER this middleware
			// returns, so c.Response().StatusCode() is still the default 200 here —
			// take the code from the error instead so failures aren't logged as 200.
			status := c.Response().StatusCode()
			if err != nil {
				status = fiber.StatusInternalServerError
				if fe, ok := err.(*fiber.Error); ok {
					status = fe.Code
				}
			}
			_ = st.addAudit(action, category, p, c.IP(), size, status)
		}
		return err
	}
}

// classify derives the audit action/target from the matched route. Ingest and
// review endpoints return "" and are not audited.
func classify(c *fiber.Ctx) (action, category, p string, size int64) {
	method, pth := c.Method(), c.Path()
	switch {
	case method == fiber.MethodPut && strings.HasPrefix(pth, "/api/files/"):
		return "upload", c.Params("category"), c.Params("*"), int64(max(0, c.Request().Header.ContentLength()))
	case method == fiber.MethodDelete && strings.HasPrefix(pth, "/api/files/"):
		return "delete", c.Params("category"), c.Params("*"), 0
	case method == fiber.MethodPost && strings.HasPrefix(pth, "/api/folders/"):
		return "mkdir", c.Params("category"), c.Params("*"), 0
	case method == fiber.MethodGet && strings.HasPrefix(pth, "/api/download/"):
		return "download", c.Params("category"), c.Params("*"), int64(max(0, c.Response().Header.ContentLength()))
	case method == fiber.MethodGet && pth == "/api/files":
		return "list", c.Query("category"), c.Query("prefix"), 0
	}
	return "", "", "", 0
}

// bearerAuth enforces a static bearer token when ADMIN_TOKEN is set.
func bearerAuth(token string) fiber.Handler {
	want := []byte(token)
	return func(c *fiber.Ctx) error {
		got := []byte(strings.TrimPrefix(c.Get("Authorization"), "Bearer "))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			return fiber.NewError(fiber.StatusUnauthorized, "unauthorized")
		}
		return c.Next()
	}
}

func newApp(dataDir, staticDir string, st *Store) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:               "windep-admin",
		DisableStartupMessage: true,
		StreamRequestBody:     true,
		BodyLimit:             32 * 1024 * 1024 * 1024,
		JSONEncoder:           json.Marshal,
	})
	app.Use(recover.New())
	app.Get("/healthz", func(c *fiber.Ctx) error { return c.SendString("ok") })

	api := app.Group("/api")
	if token := os.Getenv("ADMIN_TOKEN"); token != "" {
		api.Use(bearerAuth(token))
		slog.Info("ADMIN_TOKEN set - /api requires bearer auth")
	} else {
		slog.Warn("ADMIN_TOKEN not set - /api is unauthenticated; restrict via NetworkPolicy")
	}
	api.Use(auditMW(st))

	// review surfaces (UI)
	api.Get("/logs", handleLogs(st))
	api.Get("/audit", handleAudit(st))
	api.Get("/fleet", handleFleet(st))
	// ingest (from windep-api, best-effort)
	api.Post("/ingest/status", handleIngestStatus(st))
	api.Post("/ingest/log", handleIngestLog(st))
	// file browser
	api.Get("/files", handleList(dataDir))
	api.Put("/files/:category/*", handleUpload(dataDir))
	api.Delete("/files/:category/*", handleDelete(dataDir))
	api.Post("/folders/:category/*", handleMkdir(dataDir))
	api.Get("/download/:category/*", handleDownload(dataDir))

	app.Static("/", staticDir, fiber.Static{Index: "index.html"})
	app.Use(func(c *fiber.Ctx) error {
		return c.SendFile(filepath.Join(staticDir, "index.html"))
	})
	return app
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	dataDir := getenv("DATA_DIR", "/srv/windep")
	staticDir := getenv("STATIC_DIR", "/app/web")
	addr := getenv("LISTEN_ADDR", ":8443")

	st, err := openStore(getenv("DB_PATH", "/data/windep.db"))
	if err != nil {
		slog.Error("open store", "err", err.Error())
		os.Exit(1)
	}
	defer st.close()

	// Bound growth on the fixed DB volume: keep the newest maxRows in each table.
	maxRows := 200_000
	if n, err := strconv.Atoi(os.Getenv("DB_MAX_ROWS")); err == nil && n > 0 {
		maxRows = n
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		_ = st.prune(maxRows) // once at startup
		for range t.C {
			if err := st.prune(maxRows); err != nil {
				slog.Warn("prune failed", "err", err.Error())
			}
		}
	}()

	app := newApp(dataDir, staticDir, st)

	cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY")
	if cert != "" && key != "" {
		slog.Info("listening with TLS", "addr", addr, "dataDir", dataDir)
		err = app.ListenTLS(addr, cert, key)
	} else {
		slog.Warn("listening WITHOUT TLS (expecting upstream termination)", "addr", addr)
		err = app.Listen(addr)
	}
	if err != nil {
		slog.Error("listener stopped", "err", err.Error())
		os.Exit(1)
	}
}

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
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
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
const errNotFile = "not a file"
const errMkdir = "mkdir failed"
const errRead = "read failed"
const errWrite = "write failed"
const errNoName = "missing file name"
const errInvalidJSON = "invalid json"
const errStore = "store failed"
const logSidecar = "sidecar write failed"

type fileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	IsDir    bool   `json:"isDir"`
	Sha256   string `json:"sha256,omitempty"` // recorded at upload; from the .sha256 sidecar
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

// entryInfo converts a directory entry to a fileInfo, skipping bookkeeping files
// (integrity sidecars and in-flight upload temps) by returning ok=false.
func entryInfo(dir string, e os.DirEntry) (fileInfo, bool) {
	if strings.HasSuffix(e.Name(), sidecarExt) || strings.HasSuffix(e.Name(), partExt) {
		return fileInfo{}, false
	}
	info, err := e.Info()
	if err != nil {
		return fileInfo{}, false
	}
	fi := fileInfo{Name: e.Name(), IsDir: e.IsDir(), Modified: info.ModTime().UTC().Format("2006-01-02T15:04:05Z")}
	if !e.IsDir() {
		fi.Size = info.Size()
		fi.Sha256 = readSidecar(filepath.Join(dir, e.Name()))
	}
	return fi, true
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
			return fiber.NewError(fiber.StatusInternalServerError, errRead)
		}
		out := make([]fileInfo, 0, len(entries))
		for _, e := range entries {
			if fi, ok := entryInfo(dir, e); ok {
				out = append(out, fi)
			}
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

// copyCountedSynced streams src -> dst, fsync-ing every syncEvery bytes to keep dirty
// page cache bounded regardless of file size, and returns the number of bytes written
// (even on a mid-stream error, so a dropped upload can be resumed from where it landed).
func copyCountedSynced(dst *os.File, src io.Reader) (int64, error) {
	buf := make([]byte, 4<<20)
	var pending, total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if err := writeChunk(dst, buf[:n], &pending); err != nil {
				return total, err
			}
			total += int64(n)
		}
		if rerr == io.EOF {
			return total, dst.Sync()
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// copySynced streams src -> dst with bounded dirty cache, discarding the byte count.
func copySynced(dst *os.File, src io.Reader) error {
	_, err := copyCountedSynced(dst, src)
	return err
}

// sidecarExt names the per-file integrity sidecar (sha256sum -c compatible), written
// beside each uploaded file so integrity survives DB loss and can be re-checked (or
// verified client-side) without re-reading the file into the admin.
const sidecarExt = ".sha256"

// partExt names the in-flight staging temp. A resumable upload appends to it across
// requests; a completed upload is atomically renamed off it onto the final name.
const partExt = ".part"

// appendStream appends up to limit bytes from src to the file at path (created if
// absent), fsync-ing periodically to bound dirty page cache. It returns the bytes
// actually written so a caller can advance the resume offset even on a partial write.
func appendStream(path string, src io.Reader, limit int64) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	n, werr := copyCountedSynced(f, io.LimitReader(src, limit))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return n, werr
}

// finalizePart hashes the fully-staged temp (one streaming pass — the digest can't be
// carried across stateless, possibly-resumed chunk requests) and atomically renames it
// into place, returning the SHA-256 for the sidecar.
func finalizePart(tmp, dest string) ([]byte, error) {
	sum, err := hashFileRaw(tmp)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return nil, err
	}
	return sum, nil
}

// saveStream writes src to a ".part" temp beside dest, then atomically renames it.
// It returns the SHA-256 of the streamed bytes so the caller can record a sidecar.
func saveStream(dest string, src io.Reader) (sum []byte, err error) {
	tmp := dest + partExt
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	h := sha256.New()
	if err = copySynced(f, io.TeeReader(src, h)); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Rename(tmp, dest); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// writeSidecar records dest's digest in a sha256sum-format sidecar next to it.
func writeSidecar(dest string, sum []byte) error {
	line := fmt.Sprintf("%x  %s\n", sum, filepath.Base(dest))
	return os.WriteFile(dest+sidecarExt, []byte(line), 0o644)
}

// readSidecar returns the recorded hex digest for dest, or "" if there is none.
func readSidecar(dest string) string {
	b, err := os.ReadFile(dest + sidecarExt)
	if err != nil {
		return ""
	}
	if f := strings.Fields(string(b)); len(f) > 0 {
		return f[0]
	}
	return ""
}

// hashFileRaw computes the SHA-256 of an on-disk file, streaming it so a multi-GB WIM
// never lands in memory.
func hashFileRaw(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// hashFile is hashFileRaw as a lowercase-hex string.
func hashFile(path string) (string, error) {
	sum, err := hashFileRaw(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sum), nil
}

func handleUpload(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		rel := c.Params("*")
		if rel == "" || strings.HasSuffix(rel, "/") {
			return fiber.NewError(fiber.StatusBadRequest, errNoName)
		}
		dest, err := resolvePath(dataDir, c.Params("category"), rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, errMkdir)
		}
		sum, err := saveStream(dest, c.Context().RequestBodyStream())
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, errWrite)
		}
		// The file is already durably renamed; a sidecar failure is non-fatal.
		if err := writeSidecar(dest, sum); err != nil {
			slog.Warn(logSidecar, "path", dest, "err", err.Error())
		}
		return c.SendStatus(fiber.StatusCreated)
	}
}

// Resumable-upload headers (an offset/length protocol in the spirit of tus, kept
// minimal so it needs no extra dependency and reuses the existing .part staging file).
const (
	hdrUploadOffset   = "Upload-Offset"
	hdrUploadLength   = "Upload-Length"
	hdrUploadComplete = "Upload-Complete"
)

// handleUploadOffset answers a HEAD with how many bytes are already durable for a
// target, so the client can resume from there: the final file's size (Complete=1) if
// it already exists, else the staged .part size, else 0.
func handleUploadOffset(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		dest, err := resolvePath(dataDir, c.Params("category"), c.Params("*"))
		if err != nil {
			return err
		}
		if fi, err := os.Stat(dest); err == nil && !fi.IsDir() {
			c.Set(hdrUploadOffset, strconv.FormatInt(fi.Size(), 10))
			c.Set(hdrUploadComplete, "1")
			return c.SendStatus(fiber.StatusOK)
		}
		var off int64
		if fi, err := os.Stat(dest + partExt); err == nil {
			off = fi.Size()
		}
		c.Set(hdrUploadOffset, strconv.FormatInt(off, 10))
		c.Set(hdrUploadComplete, "0")
		return c.SendStatus(fiber.StatusOK)
	}
}

// parseResume validates a resumable PATCH's target and Upload-Offset/Upload-Length
// headers, returning the resolved dest path and the two integers.
func parseResume(c *fiber.Ctx, dataDir string) (dest string, offset, total int64, err error) {
	rel := c.Params("*")
	if rel == "" || strings.HasSuffix(rel, "/") {
		return "", 0, 0, fiber.NewError(fiber.StatusBadRequest, errNoName)
	}
	if dest, err = resolvePath(dataDir, c.Params("category"), rel); err != nil {
		return "", 0, 0, err
	}
	if total, err = strconv.ParseInt(c.Get(hdrUploadLength), 10, 64); err != nil || total < 0 {
		return "", 0, 0, fiber.NewError(fiber.StatusBadRequest, "missing or invalid Upload-Length")
	}
	if offset, err = strconv.ParseInt(c.Get(hdrUploadOffset), 10, 64); err != nil || offset < 0 {
		return "", 0, 0, fiber.NewError(fiber.StatusBadRequest, "missing or invalid Upload-Offset")
	}
	return dest, offset, total, nil
}

// handleUploadResume appends a chunk to the .part staging file at a client-declared
// offset and finalizes once it reaches Upload-Length. The offset must equal the current
// staged size, or the append would corrupt the file — a mismatch returns 409 with the
// authoritative offset so the client can re-sync (idempotent, safe to retry). Intermediate
// chunks return 204 (more expected); the finalizing chunk returns 200 with Complete=1.
func handleUploadResume(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		dest, offset, total, err := parseResume(c, dataDir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, errMkdir)
		}
		tmp := dest + partExt

		var cur int64
		if fi, err := os.Stat(tmp); err == nil {
			cur = fi.Size()
		}
		if offset != cur {
			c.Set(hdrUploadOffset, strconv.FormatInt(cur, 10))
			return fiber.NewError(fiber.StatusConflict, "offset mismatch")
		}

		// Cap the append at the remaining length so a client can't overrun the file.
		n, werr := appendStream(tmp, c.Context().RequestBodyStream(), total-cur)
		newSize := cur + n
		c.Set(hdrUploadOffset, strconv.FormatInt(newSize, 10))
		if werr != nil {
			return fiber.NewError(fiber.StatusInternalServerError, errWrite)
		}
		if newSize < total {
			c.Set(hdrUploadComplete, "0")
			return c.SendStatus(fiber.StatusNoContent) // more chunks expected
		}
		return finishUpload(c, tmp, dest)
	}
}

// finishUpload finalizes a fully-staged temp: rename into place, record the checksum
// sidecar (non-fatal on failure), and answer 200 with Complete=1.
func finishUpload(c *fiber.Ctx, tmp, dest string) error {
	sum, err := finalizePart(tmp, dest)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "finalize failed")
	}
	if err := writeSidecar(dest, sum); err != nil {
		slog.Warn(logSidecar, "path", dest, "err", err.Error())
	}
	c.Set(hdrUploadComplete, "1")
	return c.SendStatus(fiber.StatusOK)
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
			return fiber.NewError(fiber.StatusInternalServerError, errMkdir)
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
				// No final file — but an interrupted resumable upload may have left a
				// .part; discarding it lets the client cancel/abort cleanly.
				if _, perr := os.Stat(dest + partExt); perr == nil {
					os.Remove(dest + partExt)
					return c.SendStatus(fiber.StatusNoContent)
				}
				return fiber.NewError(fiber.StatusNotFound, "not found")
			}
			return fiber.NewError(fiber.StatusInternalServerError, "stat failed")
		}
		if err := os.RemoveAll(dest); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "delete failed")
		}
		os.Remove(dest + sidecarExt) // best-effort; no-op for folders
		os.Remove(dest + partExt)    // sweep any stale staging temp
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
			return fiber.NewError(fiber.StatusNotFound, errNotFile)
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

// handleWIMInfo returns the per-image catalogue embedded in a WIM (index, name,
// edition, arch, build) plus its recorded checksum, parsed from the file header —
// no DISM, so it works in the Linux admin container.
func handleWIMInfo(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		src, err := resolvePath(dataDir, c.Params("category"), c.Params("*"))
		if err != nil {
			return err
		}
		if info, err := os.Stat(src); err != nil || info.IsDir() {
			return fiber.NewError(fiber.StatusNotFound, errNotFile)
		}
		images, err := wimImages(src)
		if err != nil {
			return fiber.NewError(fiber.StatusUnprocessableEntity, err.Error())
		}
		return c.JSON(fiber.Map{"images": images, "sha256": readSidecar(src)})
	}
}

// handleVerify re-hashes a file and compares it to the checksum recorded at upload —
// the "verify before serve" gate that confirms a WIM on the PV is bit-for-bit what
// was uploaded before a machine is imaged from it.
func handleVerify(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		src, err := resolvePath(dataDir, c.Params("category"), c.Params("*"))
		if err != nil {
			return err
		}
		info, err := os.Stat(src)
		if err != nil || info.IsDir() {
			return fiber.NewError(fiber.StatusNotFound, errNotFile)
		}
		expected := readSidecar(src)
		if expected == "" {
			return fiber.NewError(fiber.StatusNotFound, "no recorded checksum")
		}
		actual, err := hashFile(src)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "hash failed")
		}
		return c.JSON(fiber.Map{"ok": actual == expected, "expected": expected, "actual": actual, "size": info.Size()})
	}
}

// --- config handlers: secret-aware read/write of the config JSON tree ---

// handleConfigGet returns a config file with every secret field masked to the KEEP
// sentinel, so the browser editor learns a credential is set without ever receiving its
// value (encrypted or plaintext). This is the only read path the editor uses.
func handleConfigGet(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		dest, err := resolvePath(dataDir, "config", c.Params("*"))
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(dest)
		if err != nil {
			if os.IsNotExist(err) {
				return fiber.NewError(fiber.StatusNotFound, "not found")
			}
			return fiber.NewError(fiber.StatusInternalServerError, errRead)
		}
		masked, err := maskConfigJSON(raw)
		if err != nil {
			return fiber.NewError(fiber.StatusUnprocessableEntity, "not valid config json")
		}
		c.Type("json")
		return c.Send(masked)
	}
}

// handleConfigPut writes a config file, encrypting secret fields at rest: a KEEP
// sentinel preserves the stored (encrypted) value, a blank clears it, and any fresh
// plaintext is encrypted with CONFIG_KEY before it ever touches the PV.
func handleConfigPut(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		rel := c.Params("*")
		if rel == "" || strings.HasSuffix(rel, "/") {
			return fiber.NewError(fiber.StatusBadRequest, errNoName)
		}
		dest, err := resolvePath(dataDir, "config", rel)
		if err != nil {
			return err
		}
		body, err := readBody(c, 1<<20)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, errRead)
		}
		prev, _ := os.ReadFile(dest) // nil when creating a new config
		out, err := encryptConfigJSON(body, prev)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid config json")
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, errMkdir)
		}
		sum, err := saveStream(dest, bytes.NewReader(out))
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, errWrite)
		}
		if err := writeSidecar(dest, sum); err != nil {
			slog.Warn(logSidecar, "path", dest, "err", err.Error())
		}
		return c.SendStatus(fiber.StatusCreated)
	}
}

// --- datastore-backed handlers: ingest (from windep-api) + review (for the UI) ---

// readBody reads the (stream-mode) request body, bounded by max so a hostile body can't
// buffer gigabytes and OOM the pod.
func readBody(c *fiber.Ctx, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(c.Context().RequestBodyStream(), max))
}

// readJSON decodes the (stream-mode) request body into v. StreamRequestBody makes
// c.Body()/BodyParser unreliable, so read the stream directly, bounded by a small
// cap so a runaway/hostile ingest body can't buffer gigabytes and OOM the pod.
func readJSON(c *fiber.Ctx, v any) error {
	body, err := readBody(c, 1<<20) // 1 MiB
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func handleIngestStatus(st *Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var r StatusReport
		if err := readJSON(c, &r); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, errInvalidJSON)
		}
		if err := st.addStatus(r); err != nil {
			slog.Error("ingest addStatus", "err", err.Error())
			return fiber.NewError(fiber.StatusInternalServerError, errStore)
		}
		return c.SendStatus(fiber.StatusAccepted)
	}
}

func handleIngestLog(st *Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var b LogBatch
		if err := readJSON(c, &b); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, errInvalidJSON)
		}
		for _, l := range b.Lines {
			if err := st.addLog(b.Serial, b.Mac, l.Level, l.Message, l.Ts); err != nil {
				slog.Error("ingest addLog", "err", err.Error())
				return fiber.NewError(fiber.StatusInternalServerError, errStore)
			}
		}
		return c.SendStatus(fiber.StatusAccepted)
	}
}

// handleIngestAudit records an audit event forwarded by windep-api — notably ZTP
// config resolutions, so credential disclosures to WinPE land in the same trail as
// file ops, attributed to the real client IP the api saw.
func handleIngestAudit(st *Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var a struct {
			Action   string `json:"action"`
			Category string `json:"category"`
			Path     string `json:"path"`
			Source   string `json:"source"`
			Size     int64  `json:"size"`
			Status   int    `json:"status"`
		}
		if err := readJSON(c, &a); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, errInvalidJSON)
		}
		if err := st.addAudit(a.Action, a.Category, a.Path, a.Source, a.Size, a.Status); err != nil {
			slog.Error("ingest addAudit", "err", err.Error())
			return fiber.NewError(fiber.StatusInternalServerError, errStore)
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
			// A resumable upload PATCHes many chunks; skip the protocol chatter —
			// intermediate 204s and 409 offset-resyncs — and record only the finalizing
			// 200 (or a genuine failure).
			if c.Method() == fiber.MethodPatch && (status == fiber.StatusNoContent || status == fiber.StatusConflict) {
				return err
			}
			_ = st.addAudit(action, category, p, c.IP(), size, status)
		}
		return err
	}
}

// classify derives the audit action/target from the matched route. Ingest and
// review endpoints return "" and are not audited.
func classify(c *fiber.Ctx) (action, category, p string, size int64) {
	const filesPrefix = "/api/files/"
	method, pth := c.Method(), c.Path()
	switch {
	case method == fiber.MethodPut && strings.HasPrefix(pth, filesPrefix):
		return "upload", c.Params("category"), c.Params("*"), int64(max(0, c.Request().Header.ContentLength()))
	case method == fiber.MethodPatch && strings.HasPrefix(pth, filesPrefix):
		total, _ := strconv.ParseInt(c.Get(hdrUploadLength), 10, 64)
		return "upload", c.Params("category"), c.Params("*"), max(0, total)
	case method == fiber.MethodDelete && strings.HasPrefix(pth, filesPrefix):
		return "delete", c.Params("category"), c.Params("*"), 0
	case method == fiber.MethodPost && strings.HasPrefix(pth, "/api/folders/"):
		return "mkdir", c.Params("category"), c.Params("*"), 0
	case method == fiber.MethodGet && strings.HasPrefix(pth, "/api/download/"):
		return "download", c.Params("category"), c.Params("*"), int64(max(0, c.Response().Header.ContentLength()))
	case method == fiber.MethodPost && strings.HasPrefix(pth, "/api/verify/"):
		return "verify", c.Params("category"), c.Params("*"), 0
	case method == fiber.MethodPut && strings.HasPrefix(pth, "/api/config/"):
		// content never logged, only the fact of a config write (creds are masked)
		return "config", "config", c.Params("*"), 0
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
	// ingest (from windep-api). Registered on the main /api only when there is NO
	// dedicated mTLS ingest listener; when INGEST_ADDR is set these are served solely
	// by newIngestApp on the mutually-authenticated port (see main()).
	if os.Getenv("INGEST_ADDR") == "" {
		registerIngest(api, st)
	}
	// file browser
	const routeFile = "/files/:category/*"
	api.Get("/files", handleList(dataDir))
	api.Put(routeFile, handleUpload(dataDir))
	api.Head(routeFile, handleUploadOffset(dataDir))  // resume: query staged offset
	api.Patch(routeFile, handleUploadResume(dataDir)) // resume: append a chunk
	api.Delete(routeFile, handleDelete(dataDir))
	api.Post("/folders/:category/*", handleMkdir(dataDir))
	api.Get("/download/:category/*", handleDownload(dataDir))
	api.Get("/wiminfo/:category/*", handleWIMInfo(dataDir))
	api.Post("/verify/:category/*", handleVerify(dataDir))
	// secret-aware config editor (masks creds on read, encrypts them on write)
	api.Get("/config/*", handleConfigGet(dataDir))
	api.Put("/config/*", handleConfigPut(dataDir))

	app.Static("/", staticDir, fiber.Static{Index: "index.html"})
	app.Use(func(c *fiber.Ctx) error {
		return c.SendFile(filepath.Join(staticDir, "index.html"))
	})
	return app
}

// registerIngest mounts the telemetry-ingest routes (posted by windep-api) on r.
func registerIngest(r fiber.Router, st *Store) {
	r.Post("/ingest/status", handleIngestStatus(st))
	r.Post("/ingest/log", handleIngestLog(st))
	r.Post("/ingest/audit", handleIngestAudit(st))
}

// newIngestApp is the dedicated ingest surface served on the mTLS-only listener. The
// listener requires+verifies a client cert (that IS the authentication), so no bearer
// token or audit middleware here — just the ingest routes.
func newIngestApp(st *Store) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:               "windep-admin-ingest",
		DisableStartupMessage: true,
		// readBody reads c.Context().RequestBodyStream(), which only works with
		// StreamRequestBody — without it RequestBodyStream() is nil and readBody panics
		// (caught by recover as a bare 500). Must match the main app.
		StreamRequestBody: true,
		BodyLimit:         8 * 1024 * 1024,
	})
	app.Use(recover.New())
	registerIngest(app.Group("/api"), st)
	return app
}

// startIngestListener starts the dedicated mTLS ingest listener in a goroutine when
// INGEST_ADDR is set (no-op otherwise). It requires+verifies client certs, so machine
// ingest is mutually authenticated while the human UI listener needs no client cert.
func startIngestListener(st *Store) {
	iaddr := os.Getenv("INGEST_ADDR")
	if iaddr == "" {
		return
	}
	cfg, err := serverTLSConfig(os.Getenv("INGEST_TLS_CERT"), os.Getenv("INGEST_TLS_KEY"), os.Getenv("INGEST_CLIENT_CA"), true)
	if err != nil {
		slog.Error("ingest tls config", "err", err.Error())
		os.Exit(1)
	}
	ln, err := tlsListener(iaddr, cfg)
	if err != nil {
		slog.Error("ingest listen", "err", err.Error())
		os.Exit(1)
	}
	iapp := newIngestApp(st)
	go func() {
		slog.Info("ingest listener (mTLS)", "addr", iaddr)
		if err := iapp.Listener(ln); err != nil {
			slog.Error("ingest listener stopped", "err", err.Error())
			os.Exit(1)
		}
	}()
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	dataDir := getenv("DATA_DIR", "/srv/windep")
	staticDir := getenv("STATIC_DIR", "/app/web")
	addr := getenv("LISTEN_ADDR", ":8443")

	switch {
	case secretsEnabled():
		slog.Info("CONFIG_KEY set - config secrets encrypted at rest")
	case os.Getenv(configKeyEnv) != "":
		slog.Warn("CONFIG_KEY is malformed (need base64 of 32 bytes) - config secrets NOT encrypted")
	default:
		slog.Warn("CONFIG_KEY not set - config secrets stored in plaintext (masked in UI only)")
	}

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

	// Dedicated mTLS ingest listener (telemetry from windep-api), when configured.
	startIngestListener(st)

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

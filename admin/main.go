// Command windep-admin is the read-write admin backend for the WinDep deploy
// server. It exposes an S3-console-style file API over the PersistentVolume
// (browse folders, upload, create folder, download, delete) and serves the
// Cloudscape SPA that drives it.
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
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// categories are the only top-level subtrees of the deploy PV the admin UI may
// touch. Folders may be nested freely beneath each.
var categories = map[string]bool{"images": true, "config": true, "boot": true}

type fileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	IsDir    bool   `json:"isDir"`
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// resolvePath maps (category, relative URL path) to an absolute filesystem path
// under DATA_DIR/<category>, neutralizing any "../" traversal. The returned path is
// guaranteed to stay within the category root.
func resolvePath(dataDir, category, rel string) (string, error) {
	if !categories[category] {
		return "", fiber.NewError(fiber.StatusBadRequest, "unknown category")
	}
	// path.Clean on a rooted path can never escape above "/", so ".." is defanged.
	clean := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(rel, "/")), "/")
	root := filepath.Join(dataDir, category)
	full := filepath.Join(root, filepath.FromSlash(clean))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fiber.NewError(fiber.StatusBadRequest, "invalid path")
	}
	return full, nil
}

// handleList returns the entries (folders first-class, via isDir) at
// <category>/<prefix>.
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
				return c.JSON([]fileInfo{}) // empty/absent folder is not an error
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
// accumulate. Writing multi-GB to the Longhorn NFS backend faster than it flushes
// piles dirty pages against the container memory cap and OOMKills the pod; periodic
// fsync forces write-back so the cache stays small and stays reclaimable.
const syncEvery = 32 << 20 // 32 MiB

// copySynced streams src -> dst, fsync-ing every syncEvery bytes to keep dirty
// page cache bounded regardless of file size.
func copySynced(dst *os.File, src io.Reader) error {
	buf := make([]byte, 4<<20) // 4 MiB read buffer
	var pending int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			if pending += int64(n); pending >= syncEvery {
				if serr := dst.Sync(); serr != nil {
					return serr
				}
				pending = 0
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

// saveStream writes src to a ".part" temp beside dest (fsync-ing periodically),
// then atomically renames it into place. On any error the temp is removed, so a
// failed upload never leaves a truncated image the read-only nginx would serve.
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

// handleUpload streams the raw request body to <category>/<path>. Streaming (not
// multipart-in-memory) plus periodic fsync is what makes multi-GB WIM uploads
// viable under a memory limit.
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
		slog.Info("uploaded", "category", c.Params("category"), "path", rel)
		return c.SendStatus(fiber.StatusCreated)
	}
}

// handleMkdir creates a folder (mkdir -p) at <category>/<path>.
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
		slog.Info("mkdir", "category", c.Params("category"), "path", rel)
		return c.SendStatus(fiber.StatusCreated)
	}
}

// handleDelete removes a file or a folder (recursively) at <category>/<path>.
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
		slog.Info("deleted", "category", c.Params("category"), "path", rel)
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// handleDownload streams a file to the browser as an attachment. It uses
// SetBodyStream with the open *os.File (an io.Closer), which fasthttp closes as
// soon as the response is sent — unlike c.Download's caching file server, which
// holds the fd ~10s. On the Longhorn RWX (NFS) volume that lingering fd triggers
// silly-rename, leaving a .nfsXXXX entry that blocks deleting the parent folder.
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
		c.Context().Response.SetBodyStream(f, int(info.Size())) // fasthttp Close()s f after send
		return nil
	}
}

// bearerAuth enforces a static bearer token when ADMIN_TOKEN is set. Network
// isolation (admin VIP NetworkPolicy) is the primary control; this is defense in depth.
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

func newApp(dataDir, staticDir string) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:               "windep-admin",
		DisableStartupMessage: true,
		StreamRequestBody:     true,                    // stream large uploads to disk
		BodyLimit:             32 * 1024 * 1024 * 1024, // 32 GiB ceiling for WIMs
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
	api.Get("/files", handleList(dataDir))
	api.Put("/files/:category/*", handleUpload(dataDir))
	api.Delete("/files/:category/*", handleDelete(dataDir))
	api.Post("/folders/:category/*", handleMkdir(dataDir))
	api.Get("/download/:category/*", handleDownload(dataDir))

	// Static Cloudscape SPA, with index.html fallback for client-side routes.
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
	app := newApp(dataDir, staticDir)

	cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY")
	var err error
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

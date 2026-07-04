// Command windep-admin is the read-write admin backend for the WinDep deploy
// server. It exposes a tiny S3-console-style file API over the PersistentVolume
// (list / upload / delete) and serves the Cloudscape SPA that drives it.
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
	"path/filepath"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// categories are the only subtrees of the deploy PV the admin UI may touch.
var categories = map[string]bool{"images": true, "config": true, "boot": true}

type fileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// safeName rejects path traversal: a valid upload/delete name is a single path
// component (no separators, no "..").
func safeName(name string) (string, bool) {
	if name == "" || name == "." || name == ".." {
		return "", false
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", false
	}
	if filepath.Base(name) != name {
		return "", false
	}
	return name, true
}

// resolve maps (category, name) to an absolute path under DATA_DIR, or errors.
func resolve(dataDir, category, name string) (string, error) {
	if !categories[category] {
		return "", fiber.NewError(fiber.StatusBadRequest, "unknown category")
	}
	clean, ok := safeName(name)
	if !ok {
		return "", fiber.NewError(fiber.StatusBadRequest, "invalid name")
	}
	return filepath.Join(dataDir, category, clean), nil
}

func handleList(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		category := c.Query("category", "images")
		if !categories[category] {
			return fiber.NewError(fiber.StatusBadRequest, "unknown category")
		}
		dir := filepath.Join(dataDir, category)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return c.JSON([]fileInfo{}) // empty category is not an error
			}
			return fiber.NewError(fiber.StatusInternalServerError, "read failed")
		}
		out := make([]fileInfo, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, fileInfo{
				Name:     e.Name(),
				Size:     info.Size(),
				Modified: info.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
			})
		}
		return c.JSON(out)
	}
}

// handleUpload streams the raw request body to <DATA_DIR>/<category>/<name>.
// Streaming (not multipart-in-memory) is what makes multi-GB WIM uploads viable.
// The write goes to a .part temp then renames, so a failed upload never leaves a
// truncated image the read-only nginx would serve.
func handleUpload(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		dest, err := resolve(dataDir, c.Params("category"), c.Params("name"))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "mkdir failed")
		}
		tmp := dest + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "create failed")
		}
		if _, err := io.Copy(f, c.Context().RequestBodyStream()); err != nil {
			f.Close()
			os.Remove(tmp)
			return fiber.NewError(fiber.StatusInternalServerError, "write failed")
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp)
			return fiber.NewError(fiber.StatusInternalServerError, "close failed")
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.Remove(tmp)
			return fiber.NewError(fiber.StatusInternalServerError, "commit failed")
		}
		slog.Info("uploaded", "category", c.Params("category"), "name", c.Params("name"))
		return c.SendStatus(fiber.StatusCreated)
	}
}

func handleDelete(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		dest, err := resolve(dataDir, c.Params("category"), c.Params("name"))
		if err != nil {
			return err
		}
		if err := os.Remove(dest); err != nil {
			if os.IsNotExist(err) {
				return fiber.NewError(fiber.StatusNotFound, "not found")
			}
			return fiber.NewError(fiber.StatusInternalServerError, "delete failed")
		}
		slog.Info("deleted", "category", c.Params("category"), "name", c.Params("name"))
		return c.SendStatus(fiber.StatusNoContent)
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
	api.Put("/files/:category/:name", handleUpload(dataDir))
	api.Delete("/files/:category/:name", handleDelete(dataDir))

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

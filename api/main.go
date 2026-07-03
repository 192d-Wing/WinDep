// Command windep-api is the WinDep deployment telemetry backend.
//
// It receives deployment status, streamed logs, and inventory from WinPE agents
// over HTTPS and emits everything as structured JSON to stdout (for cluster log
// aggregation) plus Prometheus metrics at /metrics. It is intentionally STATELESS:
// any replica can serve any machine's POST, which is what makes horizontal scaling
// and an anycast VIP correct. A small per-pod ring buffer backs the /api/machines
// debug view only and is explicitly non-authoritative.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ansrivas/fiberprometheus/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// StatusReport mirrors Deploy/Get-ZtpConfig.ps1 Send-ZtpStatus.
type StatusReport struct {
	Serial  string `json:"serial"`
	Mac     string `json:"mac"`
	State   string `json:"state"`
	Percent int    `json:"percent"`
	Message string `json:"message"`
	Model   string `json:"model"`
}

// LogBatch mirrors Deploy/Get-ZtpConfig.ps1 Send-ZtpLog.
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

type machineState struct {
	StatusReport
	Updated time.Time `json:"updated"`
}

// per-pod, non-authoritative snapshot for the debug endpoint
var (
	mu       sync.RWMutex
	machines = map[string]*machineState{}
)

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	app := fiber.New(fiber.Config{
		AppName:               "windep-api",
		DisableStartupMessage: true,
		ReadTimeout:           15 * time.Second,
		WriteTimeout:          15 * time.Second,
		BodyLimit:             8 * 1024 * 1024, // inventory payloads
	})
	app.Use(recover.New())

	prom := fiberprometheus.New("windep_api")
	prom.RegisterAt(app, "/metrics")
	app.Use(prom.Middleware)

	// Kubernetes probes.
	app.Get("/healthz", func(c *fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/readyz", func(c *fiber.Ctx) error { return c.SendString("ready") })

	// Deployment status (started/progress/succeeded/failed + policy).
	app.Post("/api/report", func(c *fiber.Ctx) error {
		var r StatusReport
		if err := json.Unmarshal(c.Body(), &r); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid json")
		}
		if r.Serial == "" {
			r.Serial = "unknown"
		}
		slog.Info("status",
			"serial", r.Serial, "mac", r.Mac, "state", r.State,
			"percent", r.Percent, "message", r.Message, "model", r.Model)
		mu.Lock()
		machines[r.Serial] = &machineState{StatusReport: r, Updated: time.Now().UTC()}
		mu.Unlock()
		return c.SendStatus(fiber.StatusNoContent)
	})

	// Streamed deploy logs (batched by the agent).
	app.Post("/api/log", func(c *fiber.Ctx) error {
		var b LogBatch
		if err := json.Unmarshal(c.Body(), &b); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid json")
		}
		for _, ln := range b.Lines {
			slog.Info("deploylog",
				"serial", b.Serial, "mac", b.Mac,
				"level", ln.Level, "ts", ln.Ts, "message", ln.Message)
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	// Full inventory upload (structured passthrough to stdout / log pipeline).
	app.Post("/api/inventory", func(c *fiber.Ctx) error {
		if !json.Valid(c.Body()) {
			return fiber.NewError(fiber.StatusBadRequest, "invalid json")
		}
		slog.Info("inventory", "bytes", strconv.Itoa(len(c.Body())), "body", json.RawMessage(c.Body()))
		return c.SendStatus(fiber.StatusNoContent)
	})

	// Debug snapshot (per-pod, non-authoritative — see package doc).
	app.Get("/api/machines", func(c *fiber.Ctx) error {
		mu.RLock()
		defer mu.RUnlock()
		out := make([]machineState, 0, len(machines))
		for _, m := range machines {
			out = append(out, *m)
		}
		return c.JSON(out)
	})

	addr := getenv("LISTEN_ADDR", ":8443")
	cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY")

	go func() {
		var err error
		if cert != "" && key != "" {
			slog.Info("listening with TLS", "addr", addr)
			err = app.ListenTLS(addr, cert, key)
		} else {
			slog.Warn("listening WITHOUT TLS (expecting TLS termination upstream)", "addr", addr)
			err = app.Listen(addr)
		}
		if err != nil {
			slog.Error("listener stopped", "err", err.Error())
			os.Exit(1)
		}
	}()

	// Graceful shutdown for clean scale-down / rollout.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = app.ShutdownWithContext(ctx)
	slog.Info("shutdown complete")
}

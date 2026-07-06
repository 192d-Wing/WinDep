// Command windep-api is the WinDep deployment telemetry backend.
//
// It receives deployment status, streamed logs, and inventory from WinPE agents
// over HTTPS and emits everything as structured JSON to stdout (for cluster log
// aggregation) plus Prometheus metrics at /metrics. It is intentionally STATELESS:
// any replica can serve any machine's POST, which is what makes horizontal scaling
// and an anycast VIP correct. A small, size-bounded per-pod map backs the
// /api/machines debug view only and is explicitly non-authoritative.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ansrivas/fiberprometheus/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

const (
	// maxMachines bounds the per-pod debug snapshot so it cannot grow without limit.
	maxMachines    = 2048
	errInvalidJSON = "invalid json"
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

// per-pod, non-authoritative, size-bounded snapshot for the debug endpoint
var (
	mu       sync.RWMutex
	machines = map[string]*machineState{}
	ready    atomic.Bool
)

// recordMachine upserts a machine, evicting the oldest entry when at capacity.
func recordMachine(r StatusReport) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := machines[r.Serial]; !exists && len(machines) >= maxMachines {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, v := range machines {
			if first || v.Updated.Before(oldest) {
				oldest, oldestKey, first = v.Updated, k, false
			}
		}
		delete(machines, oldestKey)
	}
	machines[r.Serial] = &machineState{StatusReport: r, Updated: time.Now().UTC()}
}

func snapshot() []machineState {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]machineState, 0, len(machines))
	for _, m := range machines {
		out = append(out, *m)
	}
	return out
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// --- handlers ---------------------------------------------------------------

func handleHealthz(c *fiber.Ctx) error { return c.SendString("ok") }

func handleReadyz(c *fiber.Ctx) error {
	if ready.Load() {
		return c.SendString("ready")
	}
	return c.Status(fiber.StatusServiceUnavailable).SendString("draining")
}

// forwardIngest best-effort POSTs telemetry to the admin datastore for review. It
// never blocks or fails the WinPE request: empty URL is a no-op, and the send runs
// in a goroutine bounded by the client timeout. The in-cluster hop skips TLS verify
// (the admin serving cert is for the external hostname, not its ClusterIP/DNS).
var (
	adminIngestURL   = os.Getenv("ADMIN_INGEST_URL")
	adminIngestToken = os.Getenv("ADMIN_INGEST_TOKEN") // must match admin ADMIN_TOKEN when set
	ingestClient     = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // east-west, network-isolated
		},
	}
	// Bound in-flight forwards so a slow/unreachable admin can't pile up goroutines
	// and sockets during a mass deploy; excess is dropped (telemetry is best-effort).
	ingestSem = make(chan struct{}, 64)
)

func forwardIngest(path string, payload any) {
	if adminIngestURL == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	select {
	case ingestSem <- struct{}{}:
	default:
		slog.Warn("ingest forward dropped: too many in flight")
		return
	}
	go func() {
		defer func() { <-ingestSem }()
		req, err := http.NewRequest(http.MethodPost, adminIngestURL+path, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if adminIngestToken != "" {
			req.Header.Set("Authorization", "Bearer "+adminIngestToken)
		}
		resp, err := ingestClient.Do(req)
		if err != nil {
			slog.Warn("ingest forward failed", "err", err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			slog.Warn("ingest forward rejected", "status", resp.StatusCode, "path", path)
		}
	}()
}

func handleReport(c *fiber.Ctx) error {
	var r StatusReport
	if err := json.Unmarshal(c.Body(), &r); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, errInvalidJSON)
	}
	if r.Serial == "" {
		r.Serial = "unknown"
	}
	slog.Info("status",
		"serial", r.Serial, "mac", r.Mac, "state", r.State,
		"percent", r.Percent, "message", r.Message, "model", r.Model)
	recordMachine(r)
	forwardIngest("/ingest/status", r)
	return c.SendStatus(fiber.StatusNoContent)
}

func handleLog(c *fiber.Ctx) error {
	var b LogBatch
	if err := json.Unmarshal(c.Body(), &b); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, errInvalidJSON)
	}
	for _, ln := range b.Lines {
		slog.Info("deploylog",
			"serial", b.Serial, "mac", b.Mac,
			"level", ln.Level, "ts", ln.Ts, "message", ln.Message)
	}
	forwardIngest("/ingest/log", b)
	return c.SendStatus(fiber.StatusNoContent)
}

func handleInventory(c *fiber.Ctx) error {
	if !json.Valid(c.Body()) {
		return fiber.NewError(fiber.StatusBadRequest, errInvalidJSON)
	}
	slog.Info("inventory", "bytes", len(c.Body()), "body", json.RawMessage(c.Body()))
	return c.SendStatus(fiber.StatusNoContent)
}

func handleMachines(c *fiber.Ctx) error { return c.JSON(snapshot()) }

// bearerAuth enforces a static bearer token when API_TOKEN is set (defense in
// depth for the read/debug surface; network isolation is the primary control).
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

// --- wiring -----------------------------------------------------------------

func newApp() *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:               "windep-api",
		DisableStartupMessage: true,
		ReadTimeout:           15 * time.Second,
		WriteTimeout:          15 * time.Second,
		IdleTimeout:           60 * time.Second,
		BodyLimit:             8 * 1024 * 1024, // inventory payloads
	})
	app.Use(recover.New())

	// Health probes registered BEFORE the metrics middleware so probe traffic does
	// not pollute request metrics.
	app.Get("/healthz", handleHealthz)
	app.Get("/readyz", handleReadyz)

	prom := fiberprometheus.New("windep_api")
	prom.RegisterAt(app, "/metrics")
	app.Use(prom.Middleware)

	api := app.Group("/api")
	if token := os.Getenv("API_TOKEN"); token != "" {
		api.Use(bearerAuth(token))
		slog.Info("API_TOKEN set - /api requires bearer auth")
	} else {
		slog.Warn("API_TOKEN not set - /api is unauthenticated; restrict via NetworkPolicy")
	}
	api.Post("/report", handleReport)
	api.Post("/log", handleLog)
	api.Post("/inventory", handleInventory)
	api.Get("/machines", handleMachines)
	// Zero-touch config resolution: enrollment + OPA gate, decrypts creds server-side.
	api.Post("/ztp/resolve", handleResolve(getenv("DATA_DIR", "/srv/windep")))
	return app
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// /api/ztp/resolve serves decrypted creds only when it can decrypt (CONFIG_KEY) and
	// gate (POLICY_URL). Surface both so a misconfig is obvious in the logs.
	if secretsEnabled() {
		slog.Info("CONFIG_KEY set - /api/ztp/resolve decrypts config secrets server-side")
	} else {
		slog.Warn("CONFIG_KEY not set - /api/ztp/resolve serves config as-is (encrypted values unusable)")
	}
	if os.Getenv("POLICY_URL") == "" {
		slog.Warn("POLICY_URL not set - resolve gate is enrollment-only (no OPA allow check)")
	}

	app := newApp()

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
	ready.Store(true)

	// Graceful shutdown: fail readiness first, pause for endpoint/BGP withdrawal,
	// then stop accepting and drain in-flight requests.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ready.Store(false)
	drain, err := time.ParseDuration(getenv("DRAIN_DELAY", "5s"))
	if err != nil {
		drain = 5 * time.Second
	}
	slog.Info("draining", "delay", drain.String())
	time.Sleep(drain)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = app.ShutdownWithContext(ctx)
	slog.Info("shutdown complete")
}

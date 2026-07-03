package main

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// One app for the whole test binary: fiberprometheus registers collectors on the
// global registry, so newApp must be constructed exactly once.
var app = newApp()

func status(t *testing.T, method, path, body string, headers map[string]string) int {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test(%s %s): %v", method, path, err)
	}
	return resp.StatusCode
}

func TestReport(t *testing.T) {
	if s := status(t, "POST", "/api/report", `{"serial":"S1","state":"progress","percent":10}`, nil); s != 204 {
		t.Errorf("valid report: want 204, got %d", s)
	}
	if s := status(t, "POST", "/api/report", `{not json`, nil); s != 400 {
		t.Errorf("invalid report: want 400, got %d", s)
	}
}

func TestLog(t *testing.T) {
	if s := status(t, "POST", "/api/log", `{"serial":"S1","lines":[{"level":"INFO","message":"hi"}]}`, nil); s != 204 {
		t.Errorf("valid log: want 204, got %d", s)
	}
	if s := status(t, "POST", "/api/log", `nope`, nil); s != 400 {
		t.Errorf("invalid log: want 400, got %d", s)
	}
}

func TestInventory(t *testing.T) {
	if s := status(t, "POST", "/api/inventory", `{"system":{"serial":"S1"}}`, nil); s != 204 {
		t.Errorf("valid inventory: want 204, got %d", s)
	}
	if s := status(t, "POST", "/api/inventory", `{`, nil); s != 400 {
		t.Errorf("invalid inventory: want 400, got %d", s)
	}
}

func TestMachinesSnapshot(t *testing.T) {
	if s := status(t, "GET", "/api/machines", "", nil); s != 200 {
		t.Errorf("machines: want 200, got %d", s)
	}
}

func TestHealthReady(t *testing.T) {
	if s := status(t, "GET", "/healthz", "", nil); s != 200 {
		t.Errorf("healthz: want 200, got %d", s)
	}
	ready.Store(false)
	if s := status(t, "GET", "/readyz", "", nil); s != 503 {
		t.Errorf("readyz (draining): want 503, got %d", s)
	}
	ready.Store(true)
	if s := status(t, "GET", "/readyz", "", nil); s != 200 {
		t.Errorf("readyz (ready): want 200, got %d", s)
	}
}

func TestRecordMachineEviction(t *testing.T) {
	mu.Lock()
	machines = map[string]*machineState{}
	mu.Unlock()

	for i := 0; i < maxMachines+100; i++ {
		recordMachine(StatusReport{Serial: fmt.Sprintf("S%d", i)})
	}

	mu.RLock()
	n := len(machines)
	mu.RUnlock()
	if n > maxMachines {
		t.Errorf("map exceeded cap: len=%d, max=%d", n, maxMachines)
	}
	if got := len(snapshot()); got != n {
		t.Errorf("snapshot len %d != map len %d", got, n)
	}
}

func TestBearerAuth(t *testing.T) {
	a := fiber.New()
	a.Use(bearerAuth("s3kret"))
	a.Get("/x", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	check := func(authz string, want int) {
		req := httptest.NewRequest("GET", "/x", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := a.Test(req, -1)
		if err != nil {
			t.Fatalf("Test: %v", err)
		}
		if resp.StatusCode != want {
			t.Errorf("authz %q: want %d, got %d", authz, want, resp.StatusCode)
		}
	}
	check("", fiber.StatusUnauthorized)
	check("Bearer wrong", fiber.StatusUnauthorized)
	check("Bearer s3kret", fiber.StatusOK)
}

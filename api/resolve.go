package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// /api/ztp/resolve serves a WinPE agent its decrypted config — the only place credential
// plaintext is produced, and only after two independent gates:
//  1. enrollment — an admin provisioned config/machines/<serial>.json for this machine
//  2. policy     — the api itself submits the inventory to OPA and requires "allow"
//
// The machine identity is spoofable, so it is the lookup key and audit subject, never the
// gate. Every call (served or refused) is forwarded to the admin audit trail.

var policyClient = &http.Client{Timeout: 10 * time.Second}

type ResolveRequest struct {
	Serial    string          `json:"serial"` // sanitized serial: lookup key + file name
	Mac       string          `json:"mac"`
	Model     string          `json:"model"`
	Inventory json.RawMessage `json:"inventory"` // forwarded verbatim to OPA as {input: …}
}

func handleResolve(dataDir string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req ResolveRequest
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, errInvalidJSON)
		}
		if !validSerial(req.Serial) {
			return fiber.NewError(fiber.StatusBadRequest, "invalid serial")
		}

		cfg, enrolled, err := resolveConfig(dataDir, req.Serial)
		if err != nil {
			slog.Error("resolve failed", "serial", req.Serial, "err", err.Error())
			return fiber.NewError(fiber.StatusInternalServerError, "resolve failed")
		}
		if !enrolled {
			auditResolve(c, req, "not-enrolled", fiber.StatusNotFound)
			return fiber.NewError(fiber.StatusNotFound, "not enrolled")
		}

		allow, action := evalPolicy(req.Inventory)
		if !allow {
			auditResolve(c, req, "policy-"+action, fiber.StatusForbidden)
			slog.Warn("resolve denied", "serial", req.Serial, "action", action)
			return fiber.NewError(fiber.StatusForbidden, "policy "+action)
		}

		auditResolve(c, req, "allow", fiber.StatusOK)
		slog.Info("resolve served", "serial", req.Serial, "mac", req.Mac, "model", req.Model)
		return c.JSON(cfg)
	}
}

// evalPolicy submits the inventory to OPA (POLICY_URL) and returns whether it allowed.
// Fail-closed: unreachable / non-2xx / undefined decision → the configured fail action
// (never "allow"). An empty POLICY_URL disables the gate, mirroring the WinPE client.
func evalPolicy(inventory json.RawMessage) (bool, string) {
	url := os.Getenv("POLICY_URL")
	if url == "" {
		return true, "allow"
	}
	fail := getenv("POLICY_FAIL_ACTION", "hold")
	if len(inventory) == 0 {
		inventory = json.RawMessage("null")
	}
	body, _ := json.Marshal(map[string]json.RawMessage{"input": inventory})
	resp, err := policyClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("policy engine unreachable", "err", err.Error())
		return false, fail
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("policy engine non-2xx", "status", resp.StatusCode)
		return false, fail
	}
	var parsed struct {
		Result struct {
			Action string `json:"action"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return false, fail
	}
	action := strings.ToLower(parsed.Result.Action)
	if action == "" {
		return false, fail
	}
	return action == "allow", action
}

// auditResolve records the disclosure (or refusal) in the admin audit trail; the HTTP
// status encodes the outcome (200 served / 403 policy / 404 not-enrolled).
func auditResolve(c *fiber.Ctx, req ResolveRequest, decision string, status int) {
	forwardIngest("/ingest/audit", map[string]any{
		"action":   "ztp-resolve",
		"category": "config",
		"path":     req.Serial,
		"source":   c.IP(),
		"size":     0,
		"status":   status,
	})
	slog.Info("ztp-resolve", "serial", req.Serial, "mac", req.Mac, "ip", c.IP(), "decision", decision, "status", status)
}

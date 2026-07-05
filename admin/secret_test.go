package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// withKey installs a fixed 32-byte master key for the duration of a test.
func withKey(t *testing.T) {
	t.Helper()
	prev := masterKey
	masterKey = bytes.Repeat([]byte{0x2a}, 32)
	t.Cleanup(func() { masterKey = prev })
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	withKey(t)
	plain := "P@ssw0rd! with spaces & symbols é"
	enc, err := encryptSecret(plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !isEncrypted(enc) || strings.Contains(enc, plain) {
		t.Fatalf("ciphertext leaks plaintext or missing prefix: %q", enc)
	}
	// Encryption is randomized (fresh IV) so two encryptions differ.
	enc2, _ := encryptSecret(plain)
	if enc == enc2 {
		t.Fatal("two encryptions produced identical ciphertext (IV reuse?)")
	}
	got, err := decryptSecret(enc)
	if err != nil || got != plain {
		t.Fatalf("decrypt = %q (err %v), want %q", got, err, plain)
	}
}

func TestEncryptPassthroughWhenDisabled(t *testing.T) {
	prev := masterKey
	masterKey = nil
	t.Cleanup(func() { masterKey = prev })
	if got, _ := encryptSecret("secret"); got != "secret" {
		t.Fatalf("no-key encrypt should passthrough, got %q", got)
	}
}

func TestDecryptRejectsTamper(t *testing.T) {
	withKey(t)
	enc, _ := encryptSecret("hunter2")
	// Flip a byte in the base64 body → HMAC must reject.
	b := []byte(enc)
	b[len(b)-2] ^= 0x01
	if _, err := decryptSecret(string(b)); err == nil {
		t.Fatal("tampered ciphertext should fail to decrypt")
	}
}

func TestMaskConfigJSON(t *testing.T) {
	withKey(t)
	in := `{"computerName":"WKS-1","unattend":{"TIMEZONE":"UTC","LOCALADMINUSER":"admin","LOCALADMINPASS":"plain","DOMAINPASS":"enc:v1:whatever"}}`
	out, err := maskConfigJSON([]byte(in))
	if err != nil {
		t.Fatalf("mask: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	un := m["unattend"].(map[string]any)
	if un["LOCALADMINPASS"] != keepSentinel || un["DOMAINPASS"] != keepSentinel {
		t.Fatalf("secrets not masked: %v", un)
	}
	if un["LOCALADMINUSER"] != "admin" || un["TIMEZONE"] != "UTC" || m["computerName"] != "WKS-1" {
		t.Fatalf("non-secret fields altered: %v", m)
	}
	if strings.Contains(string(out), "plain") {
		t.Fatalf("masked output still contains plaintext secret: %s", out)
	}
}

func TestEncryptConfigJSON(t *testing.T) {
	withKey(t)

	// Fresh plaintext is encrypted at rest.
	out, err := encryptConfigJSON([]byte(`{"unattend":{"LOCALADMINPASS":"newpass"}}`), nil)
	if err != nil {
		t.Fatalf("encrypt config: %v", err)
	}
	stored := unattendField(t, out, "LOCALADMINPASS")
	if !isEncrypted(stored) {
		t.Fatalf("password not encrypted at rest: %q", stored)
	}
	if dec, _ := decryptSecret(stored); dec != "newpass" {
		t.Fatalf("stored secret decrypts to %q, want newpass", dec)
	}

	// KEEP sentinel preserves the previously-stored (encrypted) value verbatim.
	prev := out
	out2, err := encryptConfigJSON([]byte(`{"unattend":{"LOCALADMINPASS":"__KEEP__"}}`), prev)
	if err != nil {
		t.Fatalf("keep: %v", err)
	}
	if got := unattendField(t, out2, "LOCALADMINPASS"); got != stored {
		t.Fatalf("KEEP did not preserve stored secret: got %q want %q", got, stored)
	}

	// Blank clears the field entirely (so it inherits default.json).
	out3, err := encryptConfigJSON([]byte(`{"unattend":{"LOCALADMINPASS":""}}`), prev)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(out3, &m)
	if un, ok := m["unattend"].(map[string]any); ok {
		if _, present := un["LOCALADMINPASS"]; present {
			t.Fatalf("blank should drop the field, still present: %v", un)
		}
	}
}

// end-to-end through the HTTP handlers: PUT stores an encrypted file on disk; GET
// returns it masked so the browser never sees the credential.
func TestConfigEndpointsMaskAndEncrypt(t *testing.T) {
	withKey(t)
	data := t.TempDir()
	app := testApp(t, data)
	url := "/api/config/machines/5CG1.json"

	body := `{"computerName":"ENG-7","unattend":{"LOCALADMINUSER":"admin","LOCALADMINPASS":"topsecret"}}`
	r, _ := app.Test(httptest.NewRequest("PUT", url, strings.NewReader(body)), -1)
	if r.StatusCode != 201 {
		t.Fatalf("config PUT: %d", r.StatusCode)
	}

	// On disk: the password is encrypted, never plaintext.
	onDisk := readFile(t, data+"/config/machines/5CG1.json")
	if strings.Contains(onDisk, "topsecret") {
		t.Fatalf("plaintext secret written to PV: %s", onDisk)
	}
	if !isEncrypted(unattendField(t, []byte(onDisk), "LOCALADMINPASS")) {
		t.Fatal("stored password is not encrypted")
	}

	// Over the wire to the editor: masked, no ciphertext or plaintext.
	r, _ = app.Test(httptest.NewRequest("GET", url, nil), -1)
	got, _ := io.ReadAll(r.Body)
	if strings.Contains(string(got), "topsecret") || strings.Contains(string(got), "enc:v1:") {
		t.Fatalf("GET leaked secret material: %s", got)
	}
	if unattendField(t, got, "LOCALADMINPASS") != keepSentinel {
		t.Fatalf("GET did not mask secret: %s", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func unattendField(t *testing.T, raw []byte, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
	un, ok := m["unattend"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := un[field].(string)
	return s
}

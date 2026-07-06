package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// encForTest mirrors admin/secret.go's encryptSecret so tests can produce ciphertext the
// api must decrypt — the same envelope the admin writes to the PV. (Live admin-binary →
// api-binary interop is exercised separately during deploy verification.)
func encForTest(plain string) string {
	block, _ := aes.NewCipher(subKey("enc"))
	iv := make([]byte, aes.BlockSize)
	for i := range iv {
		iv[i] = 0x11 // fixed IV: fine for a test, never for real encryption
	}
	n := aes.BlockSize - len(plain)%aes.BlockSize
	ct := append([]byte(plain), make([]byte, n)...)
	for i := len(plain); i < len(ct); i++ {
		ct[i] = byte(n)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, ct)
	blob := append(append([]byte{}, iv...), ct...)
	mac := hmac.New(sha256.New, subKey("mac"))
	mac.Write(blob)
	blob = mac.Sum(blob)
	return encPrefix + base64.StdEncoding.EncodeToString(blob)
}

func withKey(t *testing.T) {
	t.Helper()
	prev := masterKey
	masterKey = []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	t.Cleanup(func() { masterKey = prev })
}

func TestDecryptRoundTrip(t *testing.T) {
	withKey(t)
	enc := encForTest("D0main#P@ss")
	if !isEncrypted(enc) {
		t.Fatal("missing envelope prefix")
	}
	got, err := decryptSecret(enc)
	if err != nil || got != "D0main#P@ss" {
		t.Fatalf("decrypt = %q (err %v), want D0main#P@ss", got, err)
	}
	// Tamper → HMAC must reject.
	b := []byte(enc)
	b[len(b)-2] ^= 1
	if _, err := decryptSecret(string(b)); err == nil {
		t.Fatal("tampered ciphertext should fail")
	}
}

// writeConfig drops a config JSON under dataDir/config.
func writeConfig(t *testing.T, dataDir, rel, body string) {
	t.Helper()
	p := filepath.Join(dataDir, "config", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeOPA returns an httptest server that always answers with the given action.
func fakeOPA(t *testing.T, action string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"action":"` + action + `"}}`))
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func resolveReq(t *testing.T, app interface {
	Test(*http.Request, ...int) (*http.Response, error)
}, serial string) (*http.Response, map[string]any) {
	t.Helper()
	body := `{"serial":"` + serial + `","inventory":{"model":"OptiPlex"}}`
	r, err := app.Test(httptest.NewRequest("POST", "/api/ztp/resolve", strings.NewReader(body)), -1)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return r, m
}

func TestResolveGating(t *testing.T) {
	withKey(t)
	data := t.TempDir()
	t.Setenv("DATA_DIR", data)
	writeConfig(t, data, "default.json", `{"imageUrl":"https://x/img.wim","unattend":{"TIMEZONE":"UTC"}}`)
	writeConfig(t, data, "machines/AAA.json",
		`{"computerName":"ENG-1","unattend":{"LOCALADMINPASS":"`+encForTest("Sup3r!")+`"}}`)

	// Not enrolled → 404.
	t.Setenv("POLICY_URL", fakeOPA(t, "allow"))
	app := newApp()
	if r, _ := resolveReq(t, app, "ZZZ"); r.StatusCode != 404 {
		t.Fatalf("unenrolled: want 404, got %d", r.StatusCode)
	}

	// Enrolled + OPA allow → 200, secret decrypted, default merged in.
	r, cfg := resolveReq(t, app, "AAA")
	if r.StatusCode != 200 {
		t.Fatalf("allow: want 200, got %d", r.StatusCode)
	}
	un := cfg["unattend"].(map[string]any)
	if un["LOCALADMINPASS"] != "Sup3r!" {
		t.Fatalf("secret not decrypted: %v", un["LOCALADMINPASS"])
	}
	if un["TIMEZONE"] != "UTC" { // merged from default.json
		t.Fatalf("default not merged: %v", un)
	}
	if strings.Contains(string(mustJSON(cfg)), "enc:v1:") {
		t.Fatal("response still contains ciphertext")
	}

	// Invalid serial → 400 (path traversal guard).
	if r, _ := resolveReq(t, app, "../../etc"); r.StatusCode != 400 {
		t.Fatalf("bad serial: want 400, got %d", r.StatusCode)
	}
}

func TestResolveDeniedByPolicy(t *testing.T) {
	withKey(t)
	data := t.TempDir()
	t.Setenv("DATA_DIR", data)
	writeConfig(t, data, "machines/AAA.json", `{"unattend":{"LOCALADMINPASS":"`+encForTest("x")+`"}}`)
	t.Setenv("POLICY_URL", fakeOPA(t, "deny"))
	app := newApp()
	if r, _ := resolveReq(t, app, "AAA"); r.StatusCode != 403 {
		t.Fatalf("deny: want 403, got %d", r.StatusCode)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

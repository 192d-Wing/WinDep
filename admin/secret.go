package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// Domain-join credentials must not sit in plaintext on the shared payload PV. The admin
// encrypts secret config fields at rest; WinPE decrypts them at deploy time with the same
// key (baked into boot.wim's ztp.config.json). We use AES-256-CBC + HMAC-SHA256 rather
// than AES-GCM because the WinPE runtime is Windows PowerShell 5.1 / .NET Framework, which
// has no AesGcm — CBC+HMAC (encrypt-then-MAC) interoperates with both.
//
// Envelope:  "enc:v1:" + base64( iv[16] || ciphertext || hmac[32] )
//   encKey = HMAC-SHA256(master, "enc"),  macKey = HMAC-SHA256(master, "mac")
//   hmac   = HMAC-SHA256(macKey, iv || ciphertext)

const (
	encPrefix     = "enc:v1:"
	keepSentinel  = "__KEEP__" // UI marker: a secret is set but withheld; keep it on save
	configKeyEnv  = "CONFIG_KEY"
	secretFieldSx = "PASS" // unattend fields whose name ends in this hold secrets
)

// masterKey is the decoded CONFIG_KEY (nil when unset → encryption disabled, values are
// stored as-is). Resolved once at process start.
var masterKey = loadMasterKey()

func loadMasterKey() []byte {
	v := os.Getenv(configKeyEnv)
	if v == "" {
		return nil
	}
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || len(k) != 32 {
		// A malformed key is a config error, but must not crash the pod; log and disable.
		return nil
	}
	return k
}

func secretsEnabled() bool { return masterKey != nil }

func subKey(label string) []byte {
	h := hmac.New(sha256.New, masterKey)
	h.Write([]byte(label))
	return h.Sum(nil)
}

func isEncrypted(s string) bool { return strings.HasPrefix(s, encPrefix) }

func isSecretField(name string) bool {
	return strings.HasSuffix(strings.ToUpper(name), secretFieldSx)
}

func pkcs7Pad(b []byte, block int) []byte {
	n := block - len(b)%block
	pad := make([]byte, n)
	for i := range pad {
		pad[i] = byte(n)
	}
	return append(b, pad...)
}

func pkcs7Unpad(b []byte, block int) ([]byte, error) {
	if len(b) == 0 || len(b)%block != 0 {
		return nil, errors.New("bad padding length")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > block || n > len(b) {
		return nil, errors.New("bad padding")
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, errors.New("bad padding")
		}
	}
	return b[:len(b)-n], nil
}

// encryptSecret returns the enc:v1 envelope for plaintext, or the plaintext unchanged
// when no key is configured (so saving still works pre-key-rollout) or it's already
// encrypted/empty.
func encryptSecret(plain string) (string, error) {
	if !secretsEnabled() || plain == "" || isEncrypted(plain) {
		return plain, nil
	}
	block, err := aes.NewCipher(subKey("enc"))
	if err != nil {
		return "", err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}
	ct := pkcs7Pad([]byte(plain), aes.BlockSize)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, ct)
	blob := append(append([]byte{}, iv...), ct...)
	mac := hmac.New(sha256.New, subKey("mac"))
	mac.Write(blob)
	blob = mac.Sum(blob)
	return encPrefix + base64.StdEncoding.EncodeToString(blob), nil
}

// decryptSecret reverses encryptSecret. A non-envelope value is returned unchanged.
func decryptSecret(s string) (string, error) {
	if !isEncrypted(s) {
		return s, nil
	}
	if !secretsEnabled() {
		return "", errors.New("encrypted value but no CONFIG_KEY")
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, encPrefix))
	if err != nil {
		return "", err
	}
	if len(blob) < aes.BlockSize+sha256.Size {
		return "", errors.New("ciphertext too short")
	}
	macAt := len(blob) - sha256.Size
	iv, ct, tag := blob[:aes.BlockSize], blob[aes.BlockSize:macAt], blob[macAt:]
	mac := hmac.New(sha256.New, subKey("mac"))
	mac.Write(blob[:macAt])
	if subtle.ConstantTimeCompare(mac.Sum(nil), tag) != 1 {
		return "", errors.New("HMAC mismatch")
	}
	block, err := aes.NewCipher(subKey("enc"))
	if err != nil {
		return "", err
	}
	if len(ct)%aes.BlockSize != 0 {
		return "", errors.New("ciphertext not block-aligned")
	}
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ct)
	unpadded, err := pkcs7Unpad(out, aes.BlockSize)
	if err != nil {
		return "", err
	}
	return string(unpadded), nil
}

// trimBOM drops a leading UTF-8 byte-order mark, which a config edited in Notepad or
// written by some tooling may carry and which json.Unmarshal would otherwise reject.
func trimBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
}

// maskConfigJSON replaces every set secret field (encrypted or plaintext) with the KEEP
// sentinel so the admin UI never receives credential values — set-ness is all it needs.
func maskConfigJSON(raw []byte) ([]byte, error) {
	cfg := map[string]any{}
	if err := json.Unmarshal(trimBOM(raw), &cfg); err != nil {
		return nil, err
	}
	if un, ok := cfg["unattend"].(map[string]any); ok {
		for k, v := range un {
			if s, ok := v.(string); ok && isSecretField(k) && s != "" {
				un[k] = keepSentinel
			}
		}
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// encryptConfigJSON prepares an incoming config for storage: for each secret field it
// keeps the previously-stored value on the KEEP sentinel, drops it when cleared, and
// encrypts a freshly-entered plaintext. prevRaw is the file currently on disk (may be nil).
func encryptConfigJSON(newRaw, prevRaw []byte) ([]byte, error) {
	cfg := map[string]any{}
	if err := json.Unmarshal(trimBOM(newRaw), &cfg); err != nil {
		return nil, err
	}
	if un, ok := cfg["unattend"].(map[string]any); ok {
		prevUn := prevUnattend(prevRaw)
		for k, v := range un {
			if !isSecretField(k) {
				continue
			}
			s, _ := v.(string)
			stored, err := resolveSecret(s, prevUn[k])
			if err != nil {
				return nil, err
			}
			if stored == "" {
				delete(un, k) // cleared / nothing to keep → inherit default.json
			} else {
				un[k] = stored
			}
		}
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// resolveSecret maps an incoming secret-field value to what should be stored: KEEP
// preserves the previous (already-encrypted) value, blank clears it, and fresh plaintext
// is encrypted.
func resolveSecret(incoming string, prev any) (string, error) {
	switch {
	case incoming == keepSentinel:
		if pv, ok := prev.(string); ok {
			return pv, nil
		}
		return "", nil
	case incoming == "":
		return "", nil
	default:
		return encryptSecret(incoming)
	}
}

func prevUnattend(prevRaw []byte) map[string]any {
	if len(prevRaw) == 0 {
		return map[string]any{}
	}
	var p map[string]any
	if json.Unmarshal(prevRaw, &p) == nil {
		if un, ok := p["unattend"].(map[string]any); ok {
			return un
		}
	}
	return map[string]any{}
}

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"os"
	"strings"
)

// Config secret fields are encrypted at rest on the PV by windep-admin; the api
// decrypts them server-side to serve a resolved config to WinPE, so the key never
// leaves the server. This MUST mirror admin/secret.go's envelope byte-for-byte:
//
//	"enc:v1:" + base64( iv[16] || ciphertext || hmac[32] )
//	encKey = HMAC-SHA256(master,"enc"),  macKey = HMAC-SHA256(master,"mac")
//	hmac   = HMAC-SHA256(macKey, iv||ciphertext)   (encrypt-then-MAC)
//
// AES-256-CBC + HMAC-SHA256 (not AES-GCM) — the same choice made for WinPE compatibility;
// keeping one envelope everywhere means one thing to reason about.

const (
	encPrefix    = "enc:v1:"
	configKeyEnv = "CONFIG_KEY"
)

var masterKey = loadMasterKey()

func loadMasterKey() []byte {
	v := os.Getenv(configKeyEnv)
	if v == "" {
		return nil
	}
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || len(k) != 32 {
		return nil
	}
	return k
}

func secretsEnabled() bool { return masterKey != nil }

func isEncrypted(s string) bool { return strings.HasPrefix(s, encPrefix) }

func subKey(label string) []byte {
	h := hmac.New(sha256.New, masterKey)
	h.Write([]byte(label))
	return h.Sum(nil)
}

// decryptSecret reverses admin/secret.go's encryptSecret. A non-envelope value is
// returned unchanged, so plaintext (pre-encryption) configs still resolve.
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
	return pkcs7Unpad(out, aes.BlockSize)
}

func pkcs7Unpad(b []byte, block int) (string, error) {
	if len(b) == 0 || len(b)%block != 0 {
		return "", errors.New("bad padding length")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > block || n > len(b) {
		return "", errors.New("bad padding")
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return "", errors.New("bad padding")
		}
	}
	return string(b[:len(b)-n]), nil
}

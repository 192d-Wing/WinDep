package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// A resolved config is the same shape WinPE used to fetch statically, but assembled and
// decrypted server-side so no key ever ships in boot.wim. Enrollment == an admin created
// config/machines/<serial>.json for this exact machine.

// serialRe bounds what we'll accept as a machine serial (also the file name), so a
// hostile client can't traverse the config tree. WinPE already sanitizes to this set.
var serialRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func validSerial(s string) bool { return serialRe.MatchString(s) }

func trimBOM(b []byte) []byte { return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}) }

// readConfigFile loads and parses a config JSON under DATA_DIR/config. ok=false (nil
// error) means the file simply doesn't exist.
func readConfigFile(dataDir, rel string) (map[string]any, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "config", filepath.FromSlash(rel)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(trimBOM(raw), &m); err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// mergeConfig overlays a per-machine config on default.json (per-machine wins), merging
// the unattend block key-by-key rather than replacing it wholesale.
func mergeConfig(def, per map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range def {
		out[k] = v
	}
	for k, v := range per {
		if k != "unattend" {
			out[k] = v
		}
	}
	un := map[string]any{}
	if d, ok := def["unattend"].(map[string]any); ok {
		for k, v := range d {
			un[k] = v
		}
	}
	if p, ok := per["unattend"].(map[string]any); ok {
		for k, v := range p {
			un[k] = v
		}
	}
	if len(un) > 0 {
		out["unattend"] = un
	}
	return out
}

// decryptUnattend decrypts every enc:v1 value in the unattend block in place.
func decryptUnattend(cfg map[string]any) error {
	un, ok := cfg["unattend"].(map[string]any)
	if !ok {
		return nil
	}
	for k, v := range un {
		s, ok := v.(string)
		if !ok || !isEncrypted(s) {
			continue
		}
		dec, err := decryptSecret(s)
		if err != nil {
			return fmt.Errorf("decrypt %s: %w", k, err)
		}
		un[k] = dec
	}
	return nil
}

// resolveConfig assembles the decrypted config for a serial: default.json overlaid with
// the per-machine file, secrets decrypted. enrolled=false (nil error) means no per-machine
// config exists — the machine is not provisioned for zero-touch and gets no credentials.
func resolveConfig(dataDir, serial string) (cfg map[string]any, enrolled bool, err error) {
	per, ok, err := readConfigFile(dataDir, "machines/"+serial+".json")
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	def, _, err := readConfigFile(dataDir, "default.json")
	if err != nil {
		return nil, false, err
	}
	merged := mergeConfig(def, per)
	if err := decryptUnattend(merged); err != nil {
		return nil, true, err
	}
	return merged, true, nil
}

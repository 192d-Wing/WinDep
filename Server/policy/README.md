# WinDep policy engine (Open Policy Agent)

The deployment is **hard-gated** by OPA. At boot, WinPE collects a hardware/firmware
inventory (conforming to [`Schema/inventory.schema.json`](../../Schema/inventory.schema.json))
and POSTs it to OPA. A `deny`/`hold` decision blocks the wipe and shows the failed
checks + remediations; only `allow` proceeds.

```
WinPE ──HTTPS POST { "input": <inventory> }──▶ OPA  /v1/data/windep/decision
      ◀── { "result": { action, allow, reasons[], remediations[] } } ──
```

## Endpoint ↔ policy mapping

`windep.rego` declares `package windep` and a rule named `decision`, so OPA exposes it at:

```
POST https://<opa-host>:8181/v1/data/windep/decision
```

Set that URL as `policyUrl` in [`Deploy/ztp.config.json`](../../Deploy/ztp.config.json)
(editable on the USB — no WinPE rebuild to change the endpoint).

## Run OPA with the policy (HTTPS, internal CA)

```bash
# 1. get OPA (>= 0.59; uses `import rego.v1`)
#    https://www.openpolicyagent.org/docs/latest/#running-opa

# 2. serve the policy over HTTPS with a cert chaining to your internal CA
opa run --server \
  --tls-cert-file /etc/opa/opa.crt \
  --tls-private-key-file /etc/opa/opa.key \
  --addr 0.0.0.0:8181 \
  windep.rego
```

WinPE trusts the internal CA (baked in at build time), so the TLS handshake validates.
For high availability, run OPA behind your existing HTTPS load balancer / reverse proxy.

## Test it

```bash
# allow case
curl -sk https://opa.jhics.org:8181/v1/data/windep/decision \
  -d '{"input":{"system":{"model":"OptiPlex 7010"},
       "firmware":{"firmwareType":"UEFI","secureBoot":"on","biosVersion":"1.30.0"},
       "security":{"tpmPresent":true,"tpmVersion":"2.0"},
       "memory":{"totalGB":16},
       "storage":{"disks":[{"sizeGB":476,"mediaType":"NVMe"}]}}}' | jq .result
# -> { "action": "allow", "allow": true, "reasons": [], "remediations": [] }

# deny case (Secure Boot off, old BIOS, no TPM)
curl -sk https://opa.jhics.org:8181/v1/data/windep/decision \
  -d '{"input":{"system":{"model":"OptiPlex 7010"},
       "firmware":{"firmwareType":"UEFI","secureBoot":"off","biosVersion":"1.10.0"},
       "security":{"tpmPresent":false},
       "memory":{"totalGB":4},
       "storage":{"disks":[{"sizeGB":120}]}}}' | jq .result
```

## Editing the policy

Tune the tables at the top of [`windep.rego`](windep.rego): `allowed_models`,
`min_ram_gb`, `min_disk_gb`, `min_bios`. Add new `violations contains v if { ... }`
blocks for any rule; each contributes a `reason` (shown to the technician) and a
`remediation` (the fix). No client changes are needed — the agent just renders whatever
`reasons`/`remediations` come back.

## Notes & caveats

- **Fail-closed.** If OPA is unreachable or returns no decision, the agent applies the
  `policyFailAction` from `ztp.config.json` (default `hold`) — it does **not** deploy.
- **BIOS versioning.** `semver.compare` expects semver-like strings (`1.28.0`). Some
  OEM BIOS versions aren't semver (e.g. Dell `A28`); for those, replace the `min_bios`
  check with a per-vendor comparison or a known-good allowlist.
- **Optional config from policy.** If a rule adds a `"config"` object to the `decision`,
  the agent overlays it on the resolved deployment config (computer name, target disk,
  image URL, unattend values) — letting policy steer deployment, not just gate it.
- **Disable the gate.** Leave `policyUrl` empty in `ztp.config.json` to skip evaluation
  (returns `allow`) during early rollout.

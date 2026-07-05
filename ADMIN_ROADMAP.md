# WinDep Admin Interface — Roadmap

The admin interface (`admin/`, image `ghcr.io/192d-wing/windep-admin`) is the
read-write control plane for the deploy server: it manages the payload PV (WIMs,
config, boot), shows deployment telemetry, and keeps an audit trail. This file
tracks what's done and what's next.

## Done

- **File browser** — upload (with live progress), download, create-folder, delete,
  nested-folder navigation over the RWX PV. Streams multi-GB WIMs with periodic
  fsync to stay within the memory cap.
- **Deployment logs** — `windep-api` forwards WinPE status/log telemetry to a
  lightweight SQLite datastore; reviewable with a per-serial filter.
- **Audit trail** — every file op (upload/delete/mkdir/download/list) recorded with
  time, action, target, source IP, and HTTP status. Hourly pruning bounds growth.
- **Dark mode** — Cloudscape light/dark toggle, persisted, OS-default.
- **Per-machine config editor** — Machines tab: validated form for
  `config/machines/<SERIAL>.json` (sparse override of default.json, masked creds).
  A domain-join toggle shows/hides the AD fields (and omits them when off); timezone
  is a curated `tzutil` pick-list and image URL is a dropdown of the `.wim`s discovered
  on the payload PV (URL derived from default.json's origin).
- **Live fleet dashboard** — Fleet tab: auto-refreshing board of the latest status
  per machine (state, %, model, last-seen) with imaging/succeeded/failed tallies,
  computed from the datastore.
- **WIM integrity & metadata** — every upload is SHA-256'd in-stream to a
  `sha256sum -c`-compatible `.sha256` sidecar (hidden from the browser, surfaced per
  file in the list). A per-file Details modal shows the recorded checksum and, parsed
  straight from the WIM header/XML (no DISM — the admin container is Linux), the image
  catalogue: index, name, edition, arch, and build. A "Verify before serve" action
  re-hashes on disk and flags any drift from the recorded digest; verifies are audited.
- **Resumable uploads** — an offset protocol (in the spirit of tus) over the existing
  `.part` staging file: HEAD reports how many bytes are already durable, PATCH appends
  the remaining slice at a validated offset (409 + authoritative offset on mismatch),
  and the finalizing chunk hashes + renames + writes the sidecar. The browser resumes a
  dropped upload from the server's confirmed offset with bounded, backing-off retries
  instead of restarting from zero; a cancelled upload's `.part` is swept on delete. Only
  the finalize is audited, not the per-chunk chatter.

## Next

### Tier 1 — foundational

- [ ] **Authentication & identity (+ RBAC).** *Biggest gap, now #1.* Today the
  NetworkPolicy is the only control and the audit records a source IP, not a user.
  Add **CAC/PIV mTLS** (DoD PKI client certs) or **OIDC** (Platform One SSO /
  Keycloak), then role-based access (view vs. delete vs. edit-config). Turns the
  audit trail into real per-user attribution — and puts the config editor's
  plaintext domain-join creds behind real auth.

### Tier 2 — deployment-workflow depth

- [ ] **Secret-aware config** — domain-join creds live in plaintext on the PV; move
  to k8s Secrets / encryption and mask them in the UI and audit.
- [ ] **Boot-artifact flow** — fold `Build-WinPE.ps1`'s `boot/` output (boot.wim,
  bootmgfw.efi, BCD) into the admin so the whole boot chain is managed in one place.

### Tier 3 — polish / ops

- [ ] **Live log streaming** (SSE) + log search/filter/export; pagination for
  logs/audit (currently capped at 500/1000 rows).
- [ ] **Health page** — VIP advertisement status, Longhorn capacity, TLS cert
  expiry, retention config.
- [ ] **Frontend tests** — none yet; only the Go backend is covered.
- [ ] **Flashbar toasts** for operation feedback.
- [ ] **Docs** — `Server/README.md` and `platform/README.md` still describe the old
  IIS model; update to the container/PV/datastore model.

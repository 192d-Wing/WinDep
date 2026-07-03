# WinDep — Deploy server & network boot

The server has two jobs:

1. **Serve WinPE over the network** (`/boot`) via **UEFI HTTPS Boot** and/or **TFTP PXE**.
2. **Serve deployment payloads to WinPE over HTTPS** — per-machine config, the OS image, and a status endpoint.

Everything WinPE fetches *after* it boots goes over **HTTPS to your internal CA**. Only the initial network
boot may be cleartext (TFTP fallback), and even then the loader is Microsoft-signed so Secure Boot stays intact.

---

## `wwwroot` layout

```
wwwroot/
├─ config/
│  ├─ default.json                 # fallback for any machine
│  └─ machines/<SERIAL>.json       # per-machine overrides (sanitized BIOS serial)
├─ images/
│  └─ install.wim                  # OS image (copy your 5.7 GB WIM here for network imaging)
├─ boot/                           # staged by Build-WinPE.ps1
│  ├─ bootmgfw.efi                 # MS-signed loader (Secure Boot)
│  ├─ Boot/BCD                     # ramdisk BCD -> \sources\boot.wim
│  ├─ Boot/boot.sdi
│  └─ sources/boot.wim             # the WinDep WinPE
└─ api/
   └─ report                       # status sink (optional; see below)
```

> The **unattend template lives inside `boot.wim`** (`X:\Deploy\unattend.template.xml`) and is filled in-WinPE from
> the per-machine config — so the server does not need to serve unattend files. Domain-join creds travel only inside
> the per-machine `config/machines/<SERIAL>.json` over HTTPS.

---

## 1. Host `wwwroot` over HTTPS (IIS example)

1. Copy `wwwroot` to the web root (e.g. `C:\inetpub\windep`).
2. Bind an **HTTPS** site to a cert **chaining to your internal root CA** (the same CA whose `.cer` you passed to
   `Build-WinPE.ps1 -RootCaCert`). WinPE trusts that CA, so validation succeeds.
3. Add a MIME type for `.wim` → `application/octet-stream` (IIS blocks unknown extensions by default).
4. Enable **directory-agnostic** serving of `.json`/`.efi`/`.sdi`/`.wim`. No directory browsing needed.
5. Test from a domain machine: `curl.exe https://deploy.jhics.org/config/default.json` returns the JSON.

Any web server works (nginx/Apache/Caddy) — the only hard requirement is a cert that chains to your internal CA.

---

## 2. Network boot

Both paths hand the firmware the **same MS-signed `bootmgfw.efi`**, which ramdisk-boots `\sources\boot.wim` using
`\Boot\BCD`. Route by DHCP client vendor class.

### A. UEFI HTTPS Boot (preferred — encrypted, when firmware supports it)
- The client advertises vendor class **`HTTPClient`** and (per UEFI HTTP Boot) will use its TLS store. Enroll your
  internal root CA into the firmware's TLS/HTTPS-Boot trust (via your firmware management — Dell/HP/Lenovo tooling,
  or Redfish) so the HTTPS fetch validates.
- Point the boot URL at: `https://deploy.jhics.org/boot/bootmgfw.efi`

Windows DHCP (policy on the `HTTPClient` vendor class):
```
Option 60  : "HTTPClient"                (identifies the response as HTTP boot)
Option 67  : https://deploy.jhics.org/boot/bootmgfw.efi
```

### B. TFTP PXE (fallback — firmware without HTTPS Boot)
- Serve `boot/` from a TFTP root (e.g. `\bootmgfw.efi`, `\Boot\BCD`, `\Boot\boot.sdi`, `\sources\boot.wim`).
- Windows DHCP (policy on the `PXEClient` vendor class, UEFI x64):
```
Option 66  : <tftp-server-ip>
Option 67  : bootmgfw.efi
```

> **Vendor-class routing:** create two DHCP policies — one matching Vendor Class `HTTPClient` (→ option 67 = HTTPS
> URL), one matching `PXEClient` (→ option 66/67 = TFTP). Clients that support HTTPS Boot present `HTTPClient` and get
> the encrypted path; everything else falls back to TFTP automatically.

### boot.wim transport for the ramdisk
The staged `BCD` references `[boot]\sources\boot.wim`, which resolves over the same transport the loader came from
(HTTPS for HTTPS Boot, TFTP for PXE). For very large `boot.wim` over TFTP, raise the TFTP **block size / windowsize**
on your TFTP server to keep transfer times reasonable.

---

## 3. Status endpoint (`/api/report`) — optional

`Send-ZtpStatus` POSTs small JSON updates (`serial`, `mac`, `state`, `percent`, `message`, `model`) to
`<ServerUrl>/api/report` at start/progress/success/failure. It is **best-effort and non-fatal** — if you don't stand
up an endpoint, deployment still works; you just won't get a live fleet view.

Minimal sink options:
- **IIS:** a tiny ASP.NET/handler that appends the JSON to a log or DB.
- **Anything:** any HTTP endpoint that accepts `POST application/json` at `/api/report` and returns 2xx.

Example payload:
```json
{ "serial":"5CG1234ABC", "mac":"AA:BB:CC:DD:EE:FF", "state":"progress", "percent":62, "message":"Applying Windows image  62%", "model":"OptiPlex 7010" }
```

---

## 4. Per-machine provisioning workflow

1. Deploy interactively once to learn a machine's **sanitized serial** (shown in the UI header / `windep.log`).
2. Copy `config/machines/EXAMPLE-SERIAL.json` → `config/machines/<THAT-SERIAL>.json`, set `computerName`,
   `targetDisk`, and domain-join fields.
3. Next boot, that machine deploys zero-touch to spec. Machines with no per-machine file use `default.json`.

---

## Security checklist

- [ ] HTTPS cert chains to the **internal CA** baked into `boot.wim`.
- [ ] Internal CA enrolled in firmware TLS store for **HTTPS Boot** clients.
- [ ] Deployment runs on an **isolated provisioning VLAN** (mitigates the cleartext TFTP-fallback leg).
- [ ] `config/machines/*.json` with domain creds are readable **only** over HTTPS on that VLAN; lock down the web ACLs.
- [ ] Rotate the `LOCALADMINPASS` / `svc-domainjoin` creds per policy; consider a specialize-pass script to delete
      `C:\Windows\Panther\unattend.xml` after OOBE.

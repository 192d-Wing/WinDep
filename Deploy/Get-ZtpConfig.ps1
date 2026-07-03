<#
    Get-ZtpConfig.ps1  —  Zero-Touch Provisioning: identity + HTTPS config pull.

    Resolves the deploy server from ztp.config.json (editable on the USB, no rebuild
    needed), computes machine identity, then fetches over HTTPS:
        <ServerUrl>/config/machines/<SanitizedSerial>.json   (per-machine, preferred)
        <ServerUrl>/config/default.json                      (fallback)

    Returns a normalized config object consumed by Invoke-Deploy.ps1 / the WPF UI.
    Also exposes Send-ZtpStatus to POST progress/results back to <ServerUrl>/api/report.

    HTTPS validates against the internal root CA injected into WinPE at build time.

    Config schema (server JSON, all fields optional unless noted):
      {
        "mode":         "zerotouch" | "interactive",
        "targetDisk":   "largest" | "first" | <int>,
        "computerName": "WKS-{SERIAL}",          // {SERIAL} -> sanitized serial
        "imageUrl":     "https://deploy.jhics.org/images/install.wim",
        "unattend": {
            "TIMEZONE": "Eastern Standard Time",
            "LOCALADMINUSER": "localadmin",
            "LOCALADMINPASS": "...",             // travels over HTTPS only
            "JOINDOMAIN": "jhics.org",           // empty/absent = local only
            "DOMAINOU": "OU=Workstations,DC=jhics,DC=org",
            "DOMAINUSER": "svc-domainjoin",
            "DOMAINPASS": "..."
        },
        "confirmWipe":  false                    // interactive safety prompt
      }
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch { }

$ScriptRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Import-Module (Join-Path $ScriptRoot 'DeployEngine.psm1') -Force

function Get-ZtpSettings {
    <# Reads ztp.config.json next to this script (baked into boot.wim, USB-editable). #>
    param([string]$Path = (Join-Path $ScriptRoot 'ztp.config.json'))
    if (-not (Test-Path $Path)) { throw "ztp.config.json not found at $Path" }
    Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json
}

function Invoke-JsonGet {
    param([Parameter(Mandatory)][string]$Uri)
    Add-Type -AssemblyName System.Net.Http -ErrorAction SilentlyContinue
    $client = New-Object System.Net.Http.HttpClient
    $client.Timeout = [TimeSpan]::FromSeconds(30)
    try {
        $resp = $client.GetAsync($Uri).GetAwaiter().GetResult()
        if ($resp.StatusCode -eq [System.Net.HttpStatusCode]::NotFound) { return $null }
        if (-not $resp.IsSuccessStatusCode) { throw "HTTP $([int]$resp.StatusCode) for $Uri" }
        $body = $resp.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        return ($body | ConvertFrom-Json)
    } finally { $client.Dispose() }
}

function Expand-NameTemplate {
    param([string]$Template, [pscustomobject]$Identity)
    if (-not $Template) { return $null }
    $name = $Template.Replace('{SERIAL}', $Identity.SanitizedSerial)
    # Windows computer names: <=15 chars, no illegal chars.
    $name = ($name -replace '[^A-Za-z0-9\-]', '')
    if ($name.Length -gt 15) { $name = $name.Substring(0, 15) }
    $name
}

function Get-ZtpConfig {
    <#
      Returns a normalized plan the engine understands, plus raw metadata:
        @{ Mode; DiskRule; ComputerName; ImageUrl; Unattend(hashtable);
           ConfirmWipe; Identity; ServerUrl; Source }
    #>
    [CmdletBinding()] param([switch]$Quiet)

    $settings = Get-ZtpSettings
    $server   = ($settings.serverUrl).TrimEnd('/')
    $api      = if ($settings.PSObject.Properties.Name -contains 'apiUrl' -and $settings.apiUrl) { ($settings.apiUrl).TrimEnd('/') } else { $server }
    $id       = Get-MachineIdentity
    if (-not $Quiet) { Write-Host "Identity: serial=$($id.Serial) mac=$($id.Mac)" }

    # Per-machine first, then default. Server-provided wins over ztp.config.json defaults.
    # $perMachineFound drives auto mode-selection: a per-machine file == authorized zero-touch.
    $cfg = $null; $source = $null; $perMachineFound = $false
    $perMachineUri = "$server/config/machines/$($id.SanitizedSerial).json"
    $defaultUri    = "$server/config/default.json"
    try   { $cfg = Invoke-JsonGet -Uri $perMachineUri; if ($cfg) { $source = $perMachineUri; $perMachineFound = $true } }
    catch { Write-Warning "Per-machine config fetch failed: $($_.Exception.Message)" }
    if (-not $cfg) {
        try   { $cfg = Invoke-JsonGet -Uri $defaultUri; if ($cfg) { $source = $defaultUri } }
        catch { Write-Warning "Default config fetch failed: $($_.Exception.Message)" }
    }

    # Merge helper: server value, else ztp.config.json default, else literal fallback.
    function pick($a, $b, $fallback) { if ($null -ne $a) { $a } elseif ($null -ne $b) { $b } else { $fallback } }
    $sd = $settings.defaults

    $unattend = @{}
    if ($cfg -and $cfg.PSObject.Properties.Name -contains 'unattend' -and $cfg.unattend) {
        foreach ($p in $cfg.unattend.PSObject.Properties) { $unattend[$p.Name] = $p.Value }
    }

    $nameTemplate = pick ($cfg.computerName) ($sd.computerName) 'WKS-{SERIAL}'
    $computerName = Expand-NameTemplate -Template $nameTemplate -Identity $id
    $unattend['COMPUTERNAME'] = $computerName
    if (-not $unattend.ContainsKey('SERIAL')) { $unattend['SERIAL'] = $id.SanitizedSerial }

    [pscustomobject]@{
        Mode         = pick ($cfg.mode)        ($sd.mode)        'interactive'
        DiskRule     = pick ($cfg.targetDisk)  ($sd.targetDisk)  'largest'
        ComputerName = $computerName
        ImageUrl     = pick ($cfg.imageUrl)    ($sd.imageUrl)    $null
        Unattend     = $unattend
        ConfirmWipe  = [bool](pick ($cfg.confirmWipe) ($sd.confirmWipe) $true)
        HasMachineConfig = $perMachineFound      # per-machine <serial>.json existed on the server
        Identity     = $id
        ServerUrl    = $server
        ApiUrl       = $api        # telemetry API base (report/log); falls back to serverUrl
        Source       = if ($source) { $source } else { '(no server config — using local defaults)' }
    }
}

function Merge-PolicyConfig {
    <#
      Overlay an optional "config" object returned by the policy engine onto the
      resolved ZTP config. Lets policy steer deployment (name/disk/image/unattend),
      not just gate it. Only provided keys override.
    #>
    param([Parameter(Mandatory)][pscustomobject]$Config, [Parameter(Mandatory)]$PolicyConfig)
    if (-not $PolicyConfig) { return $Config }
    $p = $PolicyConfig
    if ($p.PSObject.Properties.Name -contains 'mode'         -and $p.mode)         { $Config.Mode         = $p.mode }
    if ($p.PSObject.Properties.Name -contains 'targetDisk'   -and $p.targetDisk)   { $Config.DiskRule     = $p.targetDisk }
    if ($p.PSObject.Properties.Name -contains 'computerName' -and $p.computerName) { $Config.ComputerName = $p.computerName; $Config.Unattend['COMPUTERNAME'] = $p.computerName }
    if ($p.PSObject.Properties.Name -contains 'imageUrl'     -and $p.imageUrl)     { $Config.ImageUrl     = $p.imageUrl }
    if ($p.PSObject.Properties.Name -contains 'unattend'     -and $p.unattend) {
        foreach ($kv in $p.unattend.PSObject.Properties) { $Config.Unattend[$kv.Name] = $kv.Value }
    }
    $Config
}

function Send-ZtpJson {
    <# Best-effort JSON POST. Never throws, never writes to the host (avoids spamming
       the console during streaming). Failures are logged to windep.log only. #>
    param([string]$Uri, [string]$JsonBody, [int]$TimeoutSec = 8)
    try {
        Add-Type -AssemblyName System.Net.Http -ErrorAction SilentlyContinue
        $client = New-Object System.Net.Http.HttpClient
        $client.Timeout = [TimeSpan]::FromSeconds($TimeoutSec)
        $content = New-Object System.Net.Http.StringContent($JsonBody, [System.Text.Encoding]::UTF8, 'application/json')
        [void]$client.PostAsync($Uri, $content).GetAwaiter().GetResult()
        $client.Dispose()
    } catch {
        try { Add-Content -LiteralPath 'X:\Windows\Temp\windep.log' -Value ("{0}  [WARN] telemetry POST failed: {1}" -f (Get-Date -Format o), $_.Exception.Message) -ErrorAction SilentlyContinue } catch { }
    }
}

function Send-ZtpStatus {
    <# Best-effort status POST to <ServerUrl>/api/report. Never throws. #>
    param(
        [Parameter(Mandatory)][string]$ServerUrl,
        [Parameter(Mandatory)][pscustomobject]$Identity,
        [ValidateSet('started','progress','succeeded','failed')][string]$State,
        [int]$Percent = 0,
        [string]$Message = ''
    )
    $payload = @{
        serial  = $Identity.Serial
        mac     = $Identity.Mac
        state   = $State
        percent = $Percent
        message = $Message
        model   = $Identity.Model
    } | ConvertTo-Json -Compress
    Send-ZtpJson -Uri "$($ServerUrl.TrimEnd('/'))/api/report" -JsonBody $payload
}

function Send-ZtpLog {
    <# Best-effort batched log POST to <ServerUrl>/api/log. Accepts an array of
       "[LEVEL] message" strings; splits the level back out for the server. #>
    param(
        [Parameter(Mandatory)][string]$ServerUrl,
        [Parameter(Mandatory)][pscustomobject]$Identity,
        [Parameter(Mandatory)][string[]]$Lines
    )
    if (-not $Lines -or $Lines.Count -eq 0) { return }
    $ts = (Get-Date).ToUniversalTime().ToString('o')
    $logLines = foreach ($l in $Lines) {
        $level = 'INFO'; $msg = $l
        if ($l -match '^\[(INFO|WARN|ERROR)\]\s*(.*)$') { $level = $Matches[1]; $msg = $Matches[2] }
        @{ ts = $ts; level = $level; message = $msg }
    }
    $payload = @{
        serial = $Identity.Serial
        mac    = $Identity.Mac
        lines  = @($logLines)
    } | ConvertTo-Json -Depth 5 -Compress
    Send-ZtpJson -Uri "$($ServerUrl.TrimEnd('/'))/api/log" -JsonBody $payload
}

<#
    DeployEngine.psm1  —  WinDep core deployment engine

    UI-agnostic functions shared by the WPF GUI (DeployUI.ps1) and the headless
    zero-touch path (Invoke-Deploy.ps1). No console prompts, no [System.Windows] here.

    Progress/logging is surfaced via caller-supplied scriptblocks so the same code
    drives a WPF progress bar or a console:
        -OnProgress { param($Percent,[string]$Activity) }
        -OnLog      { param([string]$Message,[string]$Level) }   # Level: INFO|WARN|ERROR

    Target environment: WinPE (Windows PowerShell 5.1 / .NET Framework) with the
    WinPE-WMI, WinPE-NetFx, WinPE-PowerShell, WinPE-StorageWMI, WinPE-Scripting,
    WinPE-DismCmdlets optional components. The internal root CA is injected into the
    WinPE trust store at build time so HTTPS to the internal deploy server validates.

    VALIDATION CHECKLIST (run on a WinPE VM/scratch disk — NOT verified from dev env):
      [ ] Get-MachineIdentity returns a non-empty Serial on real hardware/VM
      [ ] Resolve-TargetDisk picks the expected disk for each rule (largest/index/first)
      [ ] Initialize-DeployDisk wipes + creates EFI(260)/MSR(16)/NTFS and assigns S:/W:
      [ ] Get-InstallImage finds USB-local install.wim AND downloads via HTTPS
      [ ] Invoke-ImageApply reports moving progress and applies index 1
      [ ] Write-BootFiles produces a bootable EFI System partition
      [ ] Set-DeployUnattend token-substitutes and lands unattend.xml in Panther
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# TLS 1.2 for all HTTPS in this session (WinPE default may be lower).
try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch { }

# --- internal helpers -------------------------------------------------------

function Write-DeployLog {
    param(
        [Parameter(Mandatory)][string]$Message,
        [ValidateSet('INFO','WARN','ERROR')][string]$Level = 'INFO',
        [scriptblock]$OnLog
    )
    $line = "[{0}] {1}" -f $Level, $Message
    if ($OnLog) { & $OnLog $Message $Level } else { Write-Host $line }
    # Always tee to the WinPE scratch log for post-mortem.
    try { Add-Content -LiteralPath 'X:\Windows\Temp\windep.log' -Value ("{0}  {1}" -f (Get-Date -Format o), $line) -ErrorAction SilentlyContinue } catch { }
}

function Report-Progress {
    param([int]$Percent, [string]$Activity, [scriptblock]$OnProgress)
    if ($OnProgress) { & $OnProgress $Percent $Activity }
}

# ---------------------------------------------------------------------------
# Machine identity
# ---------------------------------------------------------------------------
function Get-MachineIdentity {
    [CmdletBinding()] param()

    $bios = Get-CimInstance -ClassName Win32_BIOS -ErrorAction SilentlyContinue
    $cs   = Get-CimInstance -ClassName Win32_ComputerSystemProduct -ErrorAction SilentlyContinue

    $serial = $null
    if ($bios -and $bios.SerialNumber) { $serial = ($bios.SerialNumber).Trim() }
    # Filter out useless OEM placeholders.
    if (-not $serial -or $serial -match '^(0+|To be filled.*|Default string|System Serial Number|None)$') {
        if ($cs -and $cs.IdentifyingNumber) { $serial = ($cs.IdentifyingNumber).Trim() }
    }
    if (-not $serial) { $serial = 'UNKNOWN' }

    $mac = $null
    $nic = Get-CimInstance -ClassName Win32_NetworkAdapter -ErrorAction SilentlyContinue |
           Where-Object { $_.PhysicalAdapter -and $_.MACAddress } |
           Sort-Object -Property InterfaceIndex |
           Select-Object -First 1
    if ($nic) { $mac = $nic.MACAddress }

    [pscustomobject]@{
        Serial       = $serial
        SanitizedSerial = ($serial -replace '[^A-Za-z0-9\-]', '')   # safe for URLs / computer names
        Uuid         = if ($cs) { $cs.UUID } else { $null }
        Mac          = $mac
        Manufacturer = if ($cs) { $cs.Vendor } else { $null }
        Model        = if ($cs) { $cs.Name }   else { $null }
    }
}

# ---------------------------------------------------------------------------
# Disk selection
# ---------------------------------------------------------------------------
function Get-DeployDisks {
    # Fixed disks only, ordered by number. Excludes the boot/USB (removable) media.
    Get-CimInstance -ClassName Win32_DiskDrive -ErrorAction Stop |
        Where-Object { $_.MediaType -notmatch 'Removable' } |
        Sort-Object -Property Index |
        ForEach-Object {
            [pscustomobject]@{
                Index  = [int]$_.Index
                Model  = ($_.Model).Trim()
                SizeGB = [math]::Round($_.Size / 1GB, 0)
                Bus    = $_.InterfaceType
            }
        }
}

function Resolve-TargetDisk {
    <#
      Rule:
        'largest'  -> biggest fixed disk
        'first'    -> lowest disk number (usually disk 0)
        <integer>  -> that exact disk index
    #>
    param([Parameter(Mandatory)]$Rule)

    $disks = @(Get-DeployDisks)
    if (-not $disks) { throw "No fixed disks found to deploy to." }

    if ($Rule -is [int] -or ($Rule -is [string] -and $Rule -match '^\d+$')) {
        $idx = [int]$Rule
        $d = $disks | Where-Object { $_.Index -eq $idx } | Select-Object -First 1
        if (-not $d) { throw "Requested disk index $idx not present." }
        return $d
    }
    switch ("$Rule".ToLower()) {
        'largest' { return ($disks | Sort-Object SizeGB -Descending | Select-Object -First 1) }
        'first'   { return ($disks | Sort-Object Index         | Select-Object -First 1) }
        default   { throw "Unknown target-disk rule '$Rule' (use 'largest','first', or an index)." }
    }
}

# ---------------------------------------------------------------------------
# Partitioning  (GPT: EFI 260MB FAT32 -> S: , MSR 16MB , Windows NTFS rest -> W:)
# diskpart is used for maximum reliability across WinPE builds.
# ---------------------------------------------------------------------------
function Initialize-DeployDisk {
    param(
        [Parameter(Mandatory)][int]$DiskIndex,
        [char]$SystemLetter  = 'S',
        [char]$WindowsLetter = 'W',
        [scriptblock]$OnLog
    )
    Write-DeployLog "Partitioning disk $DiskIndex (GPT: EFI/MSR/Windows)..." 'INFO' $OnLog

    $script = @"
select disk $DiskIndex
clean
convert gpt
create partition efi size=260
format quick fs=fat32 label="System"
assign letter=$SystemLetter
create partition msr size=16
create partition primary
format quick fs=ntfs label="Windows"
assign letter=$WindowsLetter
exit
"@
    $spath = Join-Path $env:TEMP ("windep_dp_{0}.txt" -f $DiskIndex)
    Set-Content -LiteralPath $spath -Value $script -Encoding ASCII
    $p = Start-Process -FilePath 'diskpart.exe' -ArgumentList "/s `"$spath`"" -Wait -PassThru -NoNewWindow
    if ($p.ExitCode -ne 0) { throw "diskpart failed (exit $($p.ExitCode)) partitioning disk $DiskIndex." }

    if (-not (Test-Path "$WindowsLetter`:\")) { throw "Windows partition ${WindowsLetter}: did not come online." }
    if (-not (Test-Path "$SystemLetter`:\"))  { throw "System (EFI) partition ${SystemLetter}: did not come online." }

    Write-DeployLog "Disk $DiskIndex partitioned. System=${SystemLetter}: Windows=${WindowsLetter}:" 'INFO' $OnLog
    [pscustomobject]@{ SystemLetter = $SystemLetter; WindowsLetter = $WindowsLetter }
}

# ---------------------------------------------------------------------------
# HTTPS download with progress (streaming; validates against injected root CA)
# ---------------------------------------------------------------------------
function Invoke-HttpsDownload {
    param(
        [Parameter(Mandatory)][string]$Uri,
        [Parameter(Mandatory)][string]$OutFile,
        [scriptblock]$OnProgress,
        [scriptblock]$OnLog,
        [int]$TimeoutSec = 7200
    )
    Write-DeployLog "Downloading $Uri" 'INFO' $OnLog
    Add-Type -AssemblyName System.Net.Http -ErrorAction SilentlyContinue

    $handler = New-Object System.Net.Http.HttpClientHandler
    $client  = New-Object System.Net.Http.HttpClient($handler)
    $client.Timeout = [TimeSpan]::FromSeconds($TimeoutSec)
    $fs = $null; $stream = $null; $resp = $null
    try {
        $resp = $client.GetAsync($Uri, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
        if (-not $resp.IsSuccessStatusCode) { throw "HTTP $([int]$resp.StatusCode) $($resp.ReasonPhrase) for $Uri" }

        $total = $resp.Content.Headers.ContentLength
        $stream = $resp.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
        $fs = [System.IO.File]::Create($OutFile)
        $buffer = New-Object byte[] (1MB)
        $read = 0; $sofar = [int64]0; $lastPct = -1
        while (($read = $stream.Read($buffer, 0, $buffer.Length)) -gt 0) {
            $fs.Write($buffer, 0, $read)
            $sofar += $read
            if ($total -and $total -gt 0) {
                $pct = [int](($sofar / $total) * 100)
                if ($pct -ne $lastPct) {
                    Report-Progress $pct ("Downloading image  {0:N0}/{1:N0} MB" -f ($sofar/1MB), ($total/1MB)) $OnProgress
                    $lastPct = $pct
                }
            }
        }
        $fs.Flush()
        Write-DeployLog "Download complete: $OutFile ($([math]::Round((Get-Item $OutFile).Length/1GB,2)) GB)" 'INFO' $OnLog
    }
    finally {
        if ($fs)     { $fs.Dispose() }
        if ($stream) { $stream.Dispose() }
        if ($resp)   { $resp.Dispose() }
        $client.Dispose()
    }
}

# ---------------------------------------------------------------------------
# Resolve the OS image: prefer USB-local install.wim, else pull over HTTPS to
# the freshly-partitioned Windows volume (never SMB).
# ---------------------------------------------------------------------------
function Get-InstallImage {
    param(
        [string]$ImageUrl,                 # HTTPS url from ZTP config (optional)
        [char]$WindowsLetter = 'W',
        [scriptblock]$OnProgress,
        [scriptblock]$OnLog
    )
    # 1) Look for a local install.wim on any fixed/removable non-system volume (USB boot case).
    $local = Get-CimInstance Win32_LogicalDisk -ErrorAction SilentlyContinue |
             Where-Object { $_.DeviceID -and (Test-Path (Join-Path "$($_.DeviceID)\" 'install.wim')) } |
             Select-Object -First 1
    if ($local) {
        $p = Join-Path "$($local.DeviceID)\" 'install.wim'
        Write-DeployLog "Using USB-local image: $p" 'INFO' $OnLog
        return [pscustomobject]@{ Path = $p; Temporary = $false }
    }

    # 2) Network boot: download over HTTPS to the target volume, then apply locally.
    if (-not $ImageUrl) { throw "No local install.wim found and no ImageUrl provided in config." }
    $dest = "$WindowsLetter`:\install.download.wim"
    Invoke-HttpsDownload -Uri $ImageUrl -OutFile $dest -OnProgress $OnProgress -OnLog $OnLog
    return [pscustomobject]@{ Path = $dest; Temporary = $true }
}

# ---------------------------------------------------------------------------
# DISM apply with live progress (reads the raw stdout stream to catch \r-updated %)
# ---------------------------------------------------------------------------
function Invoke-ImageApply {
    param(
        [Parameter(Mandatory)][string]$ImagePath,
        [int]$Index = 1,
        [char]$WindowsLetter = 'W',
        [scriptblock]$OnProgress,
        [scriptblock]$OnLog
    )
    Write-DeployLog "Applying image index $Index to ${WindowsLetter}:\ ..." 'INFO' $OnLog

    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName  = 'dism.exe'
    $psi.Arguments = "/Apply-Image /ImageFile:`"$ImagePath`" /Index:$Index /ApplyDir:$WindowsLetter`:\"
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError  = $true
    $psi.CreateNoWindow = $true

    $proc = [System.Diagnostics.Process]::Start($psi)
    $reader = $proc.StandardOutput
    $sb = New-Object System.Text.StringBuilder
    $lastPct = -1
    while (-not $proc.HasExited -or $reader.Peek() -ge 0) {
        $ci = $reader.Read()
        if ($ci -lt 0) { Start-Sleep -Milliseconds 50; continue }
        $ch = [char]$ci
        if ($ch -eq "`r" -or $ch -eq "`n") {
            $chunk = $sb.ToString(); [void]$sb.Clear()
            if ($chunk -match '(\d{1,3}(?:\.\d+)?)%') {
                $pct = [int][double]$Matches[1]
                if ($pct -ne $lastPct) {
                    Report-Progress $pct "Applying Windows image  $pct%" $OnProgress
                    $lastPct = $pct
                }
            }
        } else {
            [void]$sb.Append($ch)
        }
    }
    $proc.WaitForExit()
    $err = $proc.StandardError.ReadToEnd()
    if ($proc.ExitCode -ne 0) { throw "DISM apply failed (exit $($proc.ExitCode)). $err" }
    Report-Progress 100 "Image applied" $OnProgress
    Write-DeployLog "Image applied successfully." 'INFO' $OnLog
}

# ---------------------------------------------------------------------------
# UEFI boot files
# ---------------------------------------------------------------------------
function Write-BootFiles {
    param(
        [char]$WindowsLetter = 'W',
        [char]$SystemLetter  = 'S',
        [scriptblock]$OnLog
    )
    Write-DeployLog "Writing UEFI boot files to ${SystemLetter}: ..." 'INFO' $OnLog
    $p = Start-Process -FilePath 'bcdboot.exe' `
            -ArgumentList "$WindowsLetter`:\Windows /s $SystemLetter`: /f UEFI" `
            -Wait -PassThru -NoNewWindow
    if ($p.ExitCode -ne 0) { throw "bcdboot failed (exit $($p.ExitCode))." }
    Write-DeployLog "Boot files written." 'INFO' $OnLog
}

# ---------------------------------------------------------------------------
# Unattend: token-substitute a template and drop it into Panther for OOBE.
# Tokens look like {COMPUTERNAME}. Values come from a hashtable.
# ---------------------------------------------------------------------------
function Set-DeployUnattend {
    param(
        [Parameter(Mandatory)][string]$TemplatePath,
        [Parameter(Mandatory)][hashtable]$Values,
        [char]$WindowsLetter = 'W',
        [scriptblock]$OnLog
    )
    if (-not (Test-Path $TemplatePath)) { throw "Unattend template not found: $TemplatePath" }
    $xml = Get-Content -LiteralPath $TemplatePath -Raw

    foreach ($k in $Values.Keys) {
        $token = '{' + $k + '}'
        $val   = [System.Security.SecurityElement]::Escape([string]$Values[$k])
        $xml   = $xml.Replace($token, $val)
    }
    # Warn on any unresolved tokens rather than shipping literal {FOO} into OOBE.
    $left = [regex]::Matches($xml, '\{[A-Z0-9_]+\}') | ForEach-Object { $_.Value } | Select-Object -Unique
    if ($left) { Write-DeployLog "Unattend has unresolved tokens: $($left -join ', ')" 'WARN' $OnLog }

    $panther = "$WindowsLetter`:\Windows\Panther"
    New-Item -ItemType Directory -Path $panther -Force | Out-Null
    $dest = Join-Path $panther 'unattend.xml'
    Set-Content -LiteralPath $dest -Value $xml -Encoding UTF8
    Write-DeployLog "unattend.xml written to $dest" 'INFO' $OnLog
    $dest
}

# ---------------------------------------------------------------------------
# Full deployment sequence used by both UI and ZTP.
#   $Plan = @{ DiskIndex; ComputerName; ImageUrl; UnattendTemplate; UnattendValues }
# Weighted progress across download/apply/bootfiles so the bar is monotonic.
# ---------------------------------------------------------------------------
function Invoke-Deployment {
    param(
        [Parameter(Mandatory)][hashtable]$Plan,
        [scriptblock]$OnProgress,
        [scriptblock]$OnLog
    )
    $stage = { param($p,$a) Report-Progress $p $a $OnProgress }

    & $stage 2 "Preparing disk $($Plan.DiskIndex)"
    $part = Initialize-DeployDisk -DiskIndex $Plan.DiskIndex -OnLog $OnLog

    # Image acquisition (download shows 0-100 mapped into 5-45 of the overall bar).
    $img = Get-InstallImage -ImageUrl $Plan.ImageUrl -WindowsLetter $part.WindowsLetter `
              -OnProgress { param($p,$a) Report-Progress ([int](5 + ($p * 0.40))) $a $OnProgress } -OnLog $OnLog

    # Apply (0-100 mapped into 45-90).
    Invoke-ImageApply -ImagePath $img.Path -Index 1 -WindowsLetter $part.WindowsLetter `
        -OnProgress { param($p,$a) Report-Progress ([int](45 + ($p * 0.45))) $a $OnProgress } -OnLog $OnLog

    if ($img.Temporary) {
        & $stage 91 "Cleaning up temporary image"
        Remove-Item -LiteralPath $img.Path -Force -ErrorAction SilentlyContinue
    }

    & $stage 93 "Writing boot files"
    Write-BootFiles -WindowsLetter $part.WindowsLetter -SystemLetter $part.SystemLetter -OnLog $OnLog

    if ($Plan.UnattendTemplate) {
        & $stage 97 "Applying unattend (OOBE automation)"
        Set-DeployUnattend -TemplatePath $Plan.UnattendTemplate -Values $Plan.UnattendValues `
            -WindowsLetter $part.WindowsLetter -OnLog $OnLog | Out-Null
    }

    & $stage 100 "Deployment complete"
    Write-DeployLog "Deployment complete on disk $($Plan.DiskIndex)." 'INFO' $OnLog
}

Export-ModuleMember -Function Get-MachineIdentity, Get-DeployDisks, Resolve-TargetDisk,
    Initialize-DeployDisk, Invoke-HttpsDownload, Get-InstallImage, Invoke-ImageApply,
    Write-BootFiles, Set-DeployUnattend, Invoke-Deployment, Write-DeployLog

<#
    Build-WinPE.ps1  —  Build the WinDep WinPE boot.wim + boot media.

    Requires the Windows ADK + WinPE add-on. Run from an ELEVATED
    "Deployment and Imaging Tools Environment" prompt (or a normal elevated prompt —
    the script auto-locates the ADK).

    What it does:
      1. Locates ADK/WinPE tooling (copype, DISM, oscdimg).
      2. copype amd64 into a working dir.
      3. Mounts boot.wim and adds optional components:
         WMI, NetFx, Scripting, PowerShell, StorageWMI, DismCmdlets, SecureStartup, Fonts.
      4. Injects Deploy\* (UI, engine, ZTP, unattend, config) + the internal root CA.
      5. Patches ztp.config.json serverUrl and installs startnet.cmd.
      6. Unmounts/commits, builds a UEFI-bootable ISO.
      7. Stages the network-boot fileset (bootmgfw.efi, boot.sdi, ramdisk BCD, boot.wim)
         into Server\wwwroot\boot for UEFI HTTPS Boot + TFTP PXE.
      8. Optionally writes a USB stick (MakeWinPEMedia).

    Example:
      .\Build-WinPE.ps1 -RootCaCert C:\certs\InternalRootCA.cer `
                        -ServerUrl https://deploy.jhics.org -OutputIso C:\out\WinDep.iso

    VALIDATION CHECKLIST (needs the ADK — not verified from dev env):
      [ ] ADK paths resolve; OC cabs found
      [ ] Mount/add-package/unmount all succeed with no dirty mounts left
      [ ] boot.wim boots to the WPF UI in a VM
      [ ] Staged BCD ramdisk-boots boot.wim over the network
#>

[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$RootCaCert,                 # internal root CA .cer to trust
    [Parameter(Mandatory)][string]$ServerUrl,                  # e.g. https://deploy.jhics.org
    [string]$OutputIso   = "$PSScriptRoot\..\out\WinDep.iso",
    [string]$WorkDir     = "$env:TEMP\WinDep_WinPE",
    [string]$DeploySrc   = "$PSScriptRoot\..\Deploy",
    [string]$StageDir    = "$PSScriptRoot\..\Server\wwwroot\boot",
    [string]$UsbDrive,                                          # optional drive letter, e.g. E
    [string]$AdkRoot                                            # override auto-detect if needed
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Info($m)  { Write-Host "[*] $m" -ForegroundColor Cyan }
function Ok($m)    { Write-Host "[+] $m" -ForegroundColor Green }
function Warn($m)  { Write-Host "[!] $m" -ForegroundColor Yellow }

# --- 0. sanity -------------------------------------------------------------
if (-not (Test-Path $RootCaCert)) { throw "RootCaCert not found: $RootCaCert" }
if (-not (Test-Path $DeploySrc))  { throw "Deploy source not found: $DeploySrc" }
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Run this from an ELEVATED prompt."
}

# --- 1. locate ADK ---------------------------------------------------------
if (-not $AdkRoot) {
    foreach ($base in @("${env:ProgramFiles(x86)}\Windows Kits\10\Assessment and Deployment Kit",
                        "$env:ProgramFiles\Windows Kits\10\Assessment and Deployment Kit")) {
        if (Test-Path $base) { $AdkRoot = $base; break }
    }
}
if (-not $AdkRoot -or -not (Test-Path $AdkRoot)) { throw "Windows ADK not found. Install the ADK + WinPE add-on." }

$WinPERoot = Join-Path $AdkRoot 'Windows Preinstallation Environment'
$copype    = Join-Path $WinPERoot 'copype.cmd'
$makemedia = Join-Path $WinPERoot 'MakeWinPEMedia.cmd'
$ocPath    = Join-Path $WinPERoot 'amd64\WinPE_OCs'
$oscdimg   = Join-Path $AdkRoot   'Deployment Tools\amd64\Oscdimg\oscdimg.exe'
foreach ($p in @($copype,$ocPath,$oscdimg)) { if (-not (Test-Path $p)) { throw "ADK component missing: $p" } }
Ok "ADK: $AdkRoot"

# copype.cmd resolves the WinPE fileset via ADK env vars (WinPERoot / OSCDImgRoot / ...)
# that the "Deployment and Imaging Tools Environment" prompt sets. Launched from a plain
# prompt those are absent and copype fails ("processor architecture was not found: amd64").
# Import them so this script works from any elevated prompt, not only the ADK shortcut.
$dandi = Join-Path $AdkRoot 'Deployment Tools\DandISetEnv.bat'
if (Test-Path $dandi) {
    Info "Importing ADK environment (DandISetEnv.bat)"
    cmd /c "call `"$dandi`" >nul 2>&1 && set" | ForEach-Object {
        if ($_ -match '^([^=]+)=(.*)$') { Set-Item -Path "env:$($Matches[1])" -Value $Matches[2] }
    }
} else {
    $env:WinPERoot = $WinPERoot # minimum copype needs
}

# --- 2. copype -------------------------------------------------------------
# Self-heal after an aborted run: a boot.wim left mounted here blocks rmdir. Discard it
# (and sweep any orphaned/corrupt mountpoints) before cleaning the work dir.
$staleMount = Join-Path $WorkDir 'mount'
if (Test-Path $staleMount) {
    Warn "Discarding stale mount from a previous run: $staleMount"
    try { Dismount-WindowsImage -Path $staleMount -Discard -ErrorAction Stop | Out-Null }
    catch { & dism.exe /Cleanup-Mountpoints | Out-Null }
}
if (Test-Path $WorkDir) { Info "Cleaning $WorkDir"; cmd /c rmdir /s /q "$WorkDir" }
Info "copype amd64 -> $WorkDir"
$copypeOut = & cmd /c "`"$copype`" amd64 `"$WorkDir`"" 2>&1
$mediaDir = Join-Path $WorkDir 'media'
$mountDir = Join-Path $WorkDir 'mount'
$bootWim  = Join-Path $mediaDir 'sources\boot.wim'
if (-not (Test-Path $bootWim)) { throw "copype did not produce boot.wim:`n$($copypeOut -join [Environment]::NewLine)" }

# --- 3. mount + optional components ---------------------------------------
Info "Mounting boot.wim"
Mount-WindowsImage -ImagePath $bootWim -Index 1 -Path $mountDir | Out-Null
$mounted = $true # cleared once committed; the finally discards if still set (i.e. we failed)

# Dependency-ordered. Each entry: base cab (+ its en-us language cab).
$ocs = 'WinPE-WMI','WinPE-NetFx','WinPE-Scripting','WinPE-PowerShell',
       'WinPE-StorageWMI','WinPE-DismCmdlets','WinPE-SecureStartup','WinPE-FontSupport-WinRE'
try {
    foreach ($oc in $ocs) {
        $base = Join-Path $ocPath "$oc.cab"
        $lang = Join-Path $ocPath "en-us\$oc`_en-us.cab"
        if (-not (Test-Path $base)) { Warn "OC not found, skipping: $oc"; continue }
        Info "Adding $oc"
        Add-WindowsPackage -Path $mountDir -PackagePath $base -ErrorAction Stop | Out-Null
        if (Test-Path $lang) { Add-WindowsPackage -Path $mountDir -PackagePath $lang | Out-Null }
    }

    # --- 4. inject payload + CA -------------------------------------------
    $destDeploy = Join-Path $mountDir 'Deploy'
    Info "Injecting Deploy payload -> $destDeploy"
    New-Item -ItemType Directory -Path $destDeploy -Force | Out-Null
    Copy-Item (Join-Path $DeploySrc '*') $destDeploy -Recurse -Force
    Copy-Item $RootCaCert (Join-Path $destDeploy 'InternalRootCA.cer') -Force

    # Patch ztp.config.json serverUrl.
    $cfgPath = Join-Path $destDeploy 'ztp.config.json'
    $cfg = Get-Content $cfgPath -Raw | ConvertFrom-Json
    $cfg.serverUrl = $ServerUrl
    if ($cfg.defaults.imageUrl -match '^https://[^/]+') {
        $cfg.defaults.imageUrl = ($cfg.defaults.imageUrl -replace '^https://[^/]+', $ServerUrl)
    }
    ($cfg | ConvertTo-Json -Depth 8) | Set-Content $cfgPath -Encoding UTF8

    # --- 5. startnet ------------------------------------------------------
    Info "Installing startnet.cmd"
    Copy-Item (Join-Path $DeploySrc 'startnet.cmd') (Join-Path $mountDir 'Windows\System32\startnet.cmd') -Force

    # --- 6. unmount (commit) ----------------------------------------------
    Info "Unmounting (commit)"
    Dismount-WindowsImage -Path $mountDir -Save | Out-Null
    $mounted = $false
    Ok "boot.wim built."
}
finally {
    # Any failure before the commit above leaves $mounted set: discard so a dangling
    # mount never blocks the next run (rmdir can't delete a mounted image directory).
    if ($mounted) {
        Warn "Build failed before commit - discarding mount."
        try { Dismount-WindowsImage -Path $mountDir -Discard | Out-Null } catch {}
    }
}

# --- 7. build ISO (UEFI, non-fatal) ---------------------------------------
# The El Torito UEFI boot image lives in the ADK Oscdimg dir, not the copype media.
# Prefer the no-prompt variant so PXE/USB boot doesn't wait on "press any key", and copy
# it into the space-free work dir so oscdimg's -bootdata path needs no embedded quoting.
# A failed ISO must not block staging the network-boot fileset (the cluster-critical part).
try {
    New-Item -ItemType Directory -Path (Split-Path $OutputIso) -Force | Out-Null
    $oscdimgDir = Split-Path $oscdimg
    $efiSrc = @('efisys_noprompt.bin','efisys.bin') |
        ForEach-Object { Join-Path $oscdimgDir $_ } | Where-Object { Test-Path $_ } | Select-Object -First 1
    if (-not $efiSrc) { throw "efisys boot image not found under $oscdimgDir." }
    $efi = Join-Path $WorkDir 'efisys.bin'
    Copy-Item $efiSrc $efi -Force
    Info "Building ISO (UEFI) -> $OutputIso"
    & $oscdimg "-bootdata:1#pEF,e,b$efi" -u2 -udfver102 -h "$mediaDir" "$OutputIso"
    if ($LASTEXITCODE -ne 0) { throw "oscdimg exit $LASTEXITCODE." }
    Ok "ISO: $OutputIso"
} catch {
    Warn "ISO build skipped/failed ($($_.Exception.Message)). Network-boot fileset is unaffected."
}

# --- 8. stage the network-boot fileset ------------------------------------
Info "Staging network-boot fileset -> $StageDir"
New-Item -ItemType Directory -Path (Join-Path $StageDir 'sources') -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $StageDir 'Boot')    -Force | Out-Null
Copy-Item $bootWim (Join-Path $StageDir 'sources\boot.wim') -Force

# bootmgfw.efi (MS-signed) + boot.sdi come from the copype media.
$srcBootMgfw = Join-Path $mediaDir 'EFI\Boot\bootx64.efi'   # this is bootmgfw renamed by copype
if (Test-Path $srcBootMgfw) { Copy-Item $srcBootMgfw (Join-Path $StageDir 'bootmgfw.efi') -Force }
$bootSdi = Join-Path $mediaDir 'Boot\boot.sdi'
if (Test-Path $bootSdi) { Copy-Item $bootSdi (Join-Path $StageDir 'Boot\boot.sdi') -Force }

# Build a ramdisk BCD that boots \sources\boot.wim.
$bcd = Join-Path $StageDir 'Boot\BCD'
if (Test-Path $bcd) { Remove-Item $bcd -Force }
Info "Generating ramdisk BCD -> $bcd"
function bcd([string]$cmd) {
    $o = & bcdedit /store "$bcd" @($cmd.Split(' ')) 2>&1
    if ($LASTEXITCODE -ne 0) { throw "bcdedit failed: $cmd`n$o" }
    return $o
}
& bcdedit /createstore "$bcd" | Out-Null
bcd "/create {ramdiskoptions} /d WinDep_Ramdisk" | Out-Null
bcd "/set {ramdiskoptions} ramdisksdidevice boot" | Out-Null
bcd "/set {ramdiskoptions} ramdisksdipath \Boot\boot.sdi" | Out-Null
$osOut = (& bcdedit /store "$bcd" /create /d "WinDep WinPE" /application osloader) 2>&1
$guid  = ([regex]'\{[0-9a-fA-F-]+\}').Match($osOut).Value
if (-not $guid) { throw "Could not parse osloader GUID from: $osOut" }
bcd "/set $guid device ramdisk=[boot]\sources\boot.wim,{ramdiskoptions}" | Out-Null
bcd "/set $guid osdevice ramdisk=[boot]\sources\boot.wim,{ramdiskoptions}" | Out-Null
bcd "/set $guid path \windows\system32\boot\winload.efi" | Out-Null
bcd "/set $guid systemroot \windows" | Out-Null
bcd "/set $guid detecthal yes" | Out-Null
bcd "/set $guid winpe yes" | Out-Null
bcd "/create {bootmgr} /d WinDep_BootMgr" | Out-Null
bcd "/set {bootmgr} timeout 0" | Out-Null
bcd "/set {bootmgr} displayorder $guid" | Out-Null
Ok "Network-boot fileset staged. See Server\README.md for DHCP wiring."

# --- 9. optional USB -------------------------------------------------------
if ($UsbDrive) {
    Warn "Writing USB $UsbDrive via MakeWinPEMedia (FAT32 - install.wim must go on a separate NTFS partition)."
    & cmd /c "`"$makemedia`" /UFD /f `"$WorkDir`" ${UsbDrive}:"
}

Ok "Done."
Write-Host ""
Write-Host "Next:"
Write-Host "  * USB imaging: copy install.wim to the USB root (or an NTFS data partition)."
Write-Host "  * Network: host Server\wwwroot over HTTPS + TFTP; wire DHCP (Server\README.md)."

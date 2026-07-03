<#
    Invoke-Deploy.ps1  —  headless entry point / orchestrator.

    Used when you want zero-touch WITHOUT the GUI (e.g. startnet.cmd decides mode,
    or you run it manually from the WinPE prompt). The WPF UI covers the interactive
    path; this covers pure automation and scripted testing.

    Usage (from WinPE):
      powershell -ExecutionPolicy Bypass -File X:\Deploy\Invoke-Deploy.ps1            # mode from server config
      powershell ... Invoke-Deploy.ps1 -Mode zerotouch                                # force zero-touch
      powershell ... Invoke-Deploy.ps1 -Mode interactive                              # launch the GUI instead
      powershell ... Invoke-Deploy.ps1 -WhatIf                                        # resolve + print plan, no wipe

    VALIDATION CHECKLIST:
      [ ] -WhatIf prints the resolved plan and touches no disk
      [ ] zerotouch honors the countdown/-Force and deploys headless
      [ ] failures return a non-zero exit code and log to X:\Windows\Temp\windep.log
#>

[CmdletBinding()]
param(
    [ValidateSet('auto','zerotouch','interactive')][string]$Mode = 'auto',
    [int]$CountdownSeconds = 10,
    [switch]$Force,      # skip the abort countdown
    [switch]$WhatIf      # resolve + display plan only
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ScriptRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Import-Module (Join-Path $ScriptRoot 'DeployEngine.psm1') -Force
. (Join-Path $ScriptRoot 'Get-ZtpConfig.ps1')
. (Join-Path $ScriptRoot 'Get-Inventory.ps1')
. (Join-Path $ScriptRoot 'Invoke-Policy.ps1')

$onLog = { param($m,$lvl) Write-Host ("[{0}] {1}" -f $lvl, $m) }
$onProg = { param($p,$a) Write-Host ("`r  {0,3}%  {1}" -f $p, $a) -NoNewline }

function Launch-Gui {
    Write-Host "Launching interactive UI..."
    & powershell.exe -NoProfile -ExecutionPolicy Bypass -File (Join-Path $ScriptRoot 'DeployUI.ps1')
    exit $LASTEXITCODE
}

try {
    $identity = Get-MachineIdentity

    # --- Phase 1: inventory ---
    Write-Host "Collecting hardware inventory..."
    $inv = Get-Inventory
    Save-Inventory -Inventory $inv | Out-Null
    Write-Host (Format-InventorySummary -Inventory $inv)

    # --- Phase 2: policy hard gate ---
    $settings = Get-ZtpSettings
    $failAction = if ($settings.PSObject.Properties.Name -contains 'policyFailAction') { $settings.policyFailAction } else { 'hold' }
    Write-Host "Evaluating deployment policy..."
    $decision = Invoke-PolicyEvaluation -PolicyUrl $settings.policyUrl -Inventory $inv -FailAction $failAction
    Write-Host ("Policy decision: {0}" -f $decision.Action.ToUpper())
    if (-not $decision.Allow) {
        Write-Host "------------------------------------------------------------"
        Write-Host "  DEPLOYMENT BLOCKED BY POLICY ($($decision.Action.ToUpper()))"
        Write-Host "  Failed checks:"
        foreach ($r in $decision.Reasons)      { Write-Host "   - $r" }
        Write-Host "  Required remediations:"
        foreach ($m in $decision.Remediations) { Write-Host "   * $m" }
        Write-Host "------------------------------------------------------------"
        $apiBase = if ($settings.PSObject.Properties.Name -contains 'apiUrl' -and $settings.apiUrl) { $settings.apiUrl } else { $settings.serverUrl }
        Send-ZtpStatus -ServerUrl $apiBase -Identity $identity `
            -State 'failed' -Message "Policy $($decision.Action): $(@($decision.Reasons) -join '; ')"
        Write-Host "No disk was touched. Exiting."
        exit 2
    }

    $cfg = Get-ZtpConfig
    if ($decision.Config) { $cfg = Merge-PolicyConfig -Config $cfg -PolicyConfig $decision.Config }
    # auto: a per-machine <serial>.json on the server authorizes zero-touch; otherwise interactive.
    if ($Mode -eq 'auto') {
        if ($cfg.HasMachineConfig -and $cfg.Mode -ne 'interactive') {
            Write-Host "Per-machine config found ($($cfg.Source)) -> zero-touch."
            $Mode = 'zerotouch'
        } else {
            Write-Host "No per-machine config for serial $($cfg.Identity.Serial) -> interactive."
            $Mode = 'interactive'
        }
    }
    if ($Mode -eq 'interactive') { Launch-Gui }

    # zero-touch (headless)
    $disk = Resolve-TargetDisk -Rule $cfg.DiskRule
    $plan = @{
        DiskIndex        = $disk.Index
        ComputerName     = $cfg.ComputerName
        ImageUrl         = $cfg.ImageUrl
        UnattendTemplate = Join-Path $ScriptRoot 'unattend.template.xml'
        UnattendValues   = $cfg.Unattend
    }

    Write-Host "============================================================"
    Write-Host "  ZERO-TOUCH DEPLOYMENT PLAN"
    Write-Host "------------------------------------------------------------"
    Write-Host ("  Serial       : {0}" -f $cfg.Identity.Serial)
    Write-Host ("  Config source: {0}" -f $cfg.Source)
    Write-Host ("  Target disk  : Disk {0}  ({1} GB, {2})" -f $disk.Index, $disk.SizeGB, $disk.Model)
    Write-Host ("  Computer name: {0}" -f $cfg.ComputerName)
    Write-Host ("  Image source : {0}" -f ($(if ($cfg.ImageUrl) { $cfg.ImageUrl } else { 'USB-local install.wim' })))
    Write-Host ("  Domain       : {0}" -f ($(if ($cfg.Unattend.JOINDOMAIN) { $cfg.Unattend.JOINDOMAIN } else { '(local only)' })))
    Write-Host "============================================================"

    if ($WhatIf) { Write-Host "WhatIf: no changes made."; exit 0 }

    if (-not $Force) {
        Write-Host ("Disk {0} will be ERASED. Ctrl+C to abort." -f $disk.Index)
        for ($s = $CountdownSeconds; $s -gt 0; $s--) {
            Write-Host ("`r  Starting in {0,2}s..." -f $s) -NoNewline; Start-Sleep -Seconds 1
        }
        Write-Host ""
    }

    Send-ZtpStatus -ServerUrl $cfg.ApiUrl -Identity $cfg.Identity -State 'started' -Message 'Headless deploy begin'

    # Log shipping: buffer lines and flush in small batches to /api/log.
    $script:logBuf = New-Object System.Collections.Generic.List[string]
    $shipFlush = { if ($cfg.ApiUrl -and $script:logBuf.Count -gt 0) {
                       Send-ZtpLog -ServerUrl $cfg.ApiUrl -Identity $cfg.Identity -Lines $script:logBuf.ToArray()
                       $script:logBuf.Clear() } }
    $onLogShip = { param($m,$lvl)
        & $onLog $m $lvl
        $script:logBuf.Add(("[{0}] {1}" -f $lvl, $m))
        if ($script:logBuf.Count -ge 12) { & $shipFlush }
    }
    # Progress: throttle status reports to every >=5% (or completion).
    $script:lastRpt = -100

    Invoke-Deployment -Plan $plan `
        -OnProgress { param($p,$a) & $onProg $p $a
                      if ($cfg.ApiUrl -and ([int]$p -ge 100 -or ([int]$p - $script:lastRpt) -ge 5)) {
                          $script:lastRpt = [int]$p
                          Send-ZtpStatus -ServerUrl $cfg.ApiUrl -Identity $cfg.Identity -State 'progress' -Percent ([int]$p) -Message $a } } `
        -OnLog $onLogShip
    & $shipFlush
    Write-Host ""
    Send-ZtpStatus -ServerUrl $cfg.ApiUrl -Identity $cfg.Identity -State 'succeeded' -Percent 100 -Message 'Complete'

    Write-Host "Deployment complete. Rebooting in 10s..."
    Start-Sleep -Seconds 10
    & wpeutil.exe reboot
    exit 0
}
catch {
    Write-Host ""
    Write-DeployLog "FATAL: $($_.Exception.Message)" 'ERROR' $onLog
    try { Send-ZtpStatus -ServerUrl $cfg.ApiUrl -Identity $cfg.Identity -State 'failed' -Message $_.Exception.Message } catch {}
    Write-Host "Deployment failed. Dropping to command prompt for diagnostics."
    exit 1
}

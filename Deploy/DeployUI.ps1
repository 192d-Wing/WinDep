<#
    DeployUI.ps1  —  WPF front-end for WinDep, launched by startnet.cmd.

    Loads DeployUI.xaml, drives interactive + zero-touch deployments, and shows live
    progress. The actual deployment runs on a background runspace (so the UI stays
    responsive); progress/log flow back through a synchronized hashtable that a
    DispatcherTimer drains onto the UI thread.

    VALIDATION CHECKLIST (WinPE VM):
      [ ] Window renders (needs WinPE-NetFx + WinPE-PowerShell in boot.wim)
      [ ] Disk combo lists fixed disks with size/model
      [ ] Interactive deploy requires typed ERASE and runs end-to-end
      [ ] Progress bar + log update live during download/apply
      [ ] Zero-touch pulls config, shows the resolved plan, counts down, deploys
      [ ] Reboot button calls wpeutil reboot
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ScriptRoot = Split-Path -Parent $MyInvocation.MyCommand.Path

Add-Type -AssemblyName PresentationFramework, PresentationCore, WindowsBase
Import-Module (Join-Path $ScriptRoot 'DeployEngine.psm1') -Force
. (Join-Path $ScriptRoot 'Get-ZtpConfig.ps1')
. (Join-Path $ScriptRoot 'Get-Inventory.ps1')
. (Join-Path $ScriptRoot 'Invoke-Policy.ps1')

# --- load XAML -------------------------------------------------------------
[xml]$xaml = Get-Content -LiteralPath (Join-Path $ScriptRoot 'DeployUI.xaml') -Raw
$reader = New-Object System.Xml.XmlNodeReader $xaml
$win    = [Windows.Markup.XamlReader]::Load($reader)

# Named-element lookup.
$ui = @{}
$xaml.SelectNodes("//*[@*[local-name()='Name']]") | ForEach-Object {
    $n = $_.Attributes['x:Name'].Value; if ($n) { $ui[$n] = $win.FindName($n) }
}

# Shared state between the worker runspace and the UI thread.
$sync = [hashtable]::Synchronized(@{
    Percent = 0; Stage = ''; LogQueue = (New-Object System.Collections.Queue);
    Done = $false; Failed = $false; Error = $null
})

$identity = Get-MachineIdentity
$ui.IdentityText.Text = "Serial: $($identity.Serial)   MAC: $($identity.Mac)"

# Telemetry API base (report/log) for ALL modes, including interactive.
$script:apiUrl = try {
    $s = Get-ZtpSettings
    if ($s.PSObject.Properties.Name -contains 'apiUrl' -and $s.apiUrl) { $s.apiUrl } else { $s.serverUrl }
} catch { $null }

# --- panel switching -------------------------------------------------------
function Show-Panel([string]$name) {
    foreach ($p in 'ModePanel','ConfigPanel','ProgressPanel','PolicyPanel') {
        $ui[$p].Visibility = if ($p -eq $name) { 'Visible' } else { 'Collapsed' }
    }
}

function Add-Log([string]$msg) {
    $ui.LogText.Text += ("{0}`n" -f $msg)
    $ui.LogScroller.ScrollToEnd()
}

# --- disk list -------------------------------------------------------------
function Load-Disks {
    $ui.DiskCombo.Items.Clear()
    foreach ($d in Get-DeployDisks) {
        $item = New-Object System.Windows.Controls.ComboBoxItem
        $item.Content = ("Disk {0}  -  {1} GB  -  {2} [{3}]" -f $d.Index, $d.SizeGB, $d.Model, $d.Bus)
        $item.Tag = $d.Index
        [void]$ui.DiskCombo.Items.Add($item)
    }
    if ($ui.DiskCombo.Items.Count -gt 0) { $ui.DiskCombo.SelectedIndex = 0 }
}

# --- background deployment worker -----------------------------------------
# $Plan is the engine hashtable; runs Invoke-Deployment on a new runspace and
# funnels progress/log into $sync. Server status is reported here too.
function Start-DeploymentWorker([hashtable]$Plan, [string]$ServerUrl) {
    Show-Panel 'ProgressPanel'
    $ui.BtnFinish.Visibility = 'Collapsed'
    $sync.Done = $false; $sync.Failed = $false; $sync.Error = $null
    $sync.Percent = 0; $sync.Stage = 'Starting'

    $rs = [runspacefactory]::CreateRunspace(); $rs.ApartmentState = 'MTA'; $rs.Open()
    $rs.SessionStateProxy.SetVariable('Plan', $Plan)
    $rs.SessionStateProxy.SetVariable('sync', $sync)
    $rs.SessionStateProxy.SetVariable('ScriptRoot', $ScriptRoot)
    $rs.SessionStateProxy.SetVariable('ServerUrl', $ServerUrl)
    $rs.SessionStateProxy.SetVariable('Identity', $identity)

    $ps = [powershell]::Create(); $ps.Runspace = $rs
    [void]$ps.AddScript({
        Import-Module (Join-Path $ScriptRoot 'DeployEngine.psm1') -Force
        . (Join-Path $ScriptRoot 'Get-ZtpConfig.ps1')
        $script:lastRpt = -100
        $onP = { param($p,$a)
            $sync.Percent = [int]$p; $sync.Stage = $a
            # Throttle: report every >=5% or at completion, so telemetry never floods
            # the API or stalls the deploy loop.
            if ($ServerUrl -and ([int]$p -ge 100 -or ([int]$p - $script:lastRpt) -ge 5)) {
                $script:lastRpt = [int]$p
                Send-ZtpStatus -ServerUrl $ServerUrl -Identity $Identity -State 'progress' -Percent ([int]$p) -Message $a
            }
        }
        $onL = { param($m,$lvl) $sync.LogQueue.Enqueue(("[{0}] {1}" -f $lvl,$m)) }
        try {
            if ($ServerUrl) { Send-ZtpStatus -ServerUrl $ServerUrl -Identity $Identity -State 'started' -Message "Deploy begin" }
            Invoke-Deployment -Plan $Plan -OnProgress $onP -OnLog $onL
            $sync.Done = $true
            if ($ServerUrl) { Send-ZtpStatus -ServerUrl $ServerUrl -Identity $Identity -State 'succeeded' -Percent 100 -Message "Complete" }
        } catch {
            $sync.Failed = $true; $sync.Error = $_.Exception.Message
            $sync.LogQueue.Enqueue("[ERROR] $($_.Exception.Message)")
            if ($ServerUrl) { Send-ZtpStatus -ServerUrl $ServerUrl -Identity $Identity -State 'failed' -Message $_.Exception.Message }
        }
    })
    $async = $ps.BeginInvoke()

    # UI-thread pump.
    $timer = New-Object System.Windows.Threading.DispatcherTimer
    $timer.Interval = [TimeSpan]::FromMilliseconds(200)
    $timer.Add_Tick({
        # Drain the log queue to the console AND batch-stream it to the API (/api/log).
        $ship = New-Object System.Collections.Generic.List[string]
        while ($sync.LogQueue.Count -gt 0) { $l = [string]$sync.LogQueue.Dequeue(); Add-Log $l; $ship.Add($l) }
        if ($ServerUrl -and $ship.Count -gt 0) { Send-ZtpLog -ServerUrl $ServerUrl -Identity $identity -Lines $ship.ToArray() }
        $ui.Bar.Value = $sync.Percent
        $ui.PercentText.Text = "$($sync.Percent)%"
        if ($sync.Stage) { $ui.StageText.Text = $sync.Stage; $ui.StatusBar.Text = $sync.Stage }
        if ($sync.Done -or $sync.Failed) {
            $timer.Stop()
            $ps.EndInvoke($async); $ps.Dispose(); $rs.Dispose()
            if ($sync.Failed) {
                $ui.StageText.Text = "Deployment FAILED"
                $ui.StageText.Foreground = '#F2B8B8'
                $ui.BtnFinish.Content = 'Exit to Command Prompt'
            } else {
                $ui.StageText.Text = "Deployment complete"
                $ui.StageText.Foreground = '#8BA7CC'
                $ui.BtnFinish.Content = 'Reboot now'
            }
            $ui.BtnFinish.Visibility = 'Visible'
            if (-not $sync.Failed) { Start-RebootCountdown 10 }
        }
    })
    $timer.Start()
}

# --- auto-reboot countdown after a successful deploy -----------------------
function Start-RebootCountdown([int]$Seconds = 10) {
    $script:rbSecs = $Seconds
    $ui.StageText.Text = "Deployment complete - rebooting in $script:rbSecs s"
    $ui.StatusBar.Text = "Rebooting in $script:rbSecs s (Reboot now to skip)"
    $rt = New-Object System.Windows.Threading.DispatcherTimer
    $rt.Interval = [TimeSpan]::FromSeconds(1)
    $rt.Add_Tick({
        $script:rbSecs--
        if ($script:rbSecs -le 0) {
            $rt.Stop()
            Start-Process 'wpeutil.exe' -ArgumentList 'reboot'
        } else {
            $ui.StageText.Text = "Deployment complete - rebooting in $script:rbSecs s"
            $ui.StatusBar.Text = "Rebooting in $script:rbSecs s (Reboot now to skip)"
        }
    })
    $rt.Start()
    $script:rebootTimer = $rt
}

# --- build an interactive plan from the form ------------------------------
function New-InteractivePlan {
    $sel = $ui.DiskCombo.SelectedItem
    if (-not $sel) { throw "Select a disk." }
    $diskIndex = [int]$sel.Tag
    $name = ($ui.NameBox.Text).Trim()
    if (-not $name) { $name = "WKS-$($identity.SanitizedSerial)" }

    $plan = @{
        DiskIndex        = $diskIndex
        ComputerName     = $name
        ImageUrl         = $null   # interactive prefers USB-local image
        UnattendTemplate = $null
        UnattendValues   = @{}
    }
    if ($ui.UnattendCheck.IsChecked) {
        # Local admin password is entered by the operator (no default baked in).
        $adminPass = $ui.AdminPassBox.Password
        if ([string]::IsNullOrEmpty($adminPass)) {
            throw "Enter a local admin password (required for unattend), or uncheck 'Apply unattend'."
        }
        $plan.UnattendTemplate = Join-Path $ScriptRoot 'unattend.template.xml'
        $plan.UnattendValues = @{
            COMPUTERNAME   = $name
            TIMEZONE       = 'Eastern Standard Time'
            LOCALADMINUSER = 'localadmin'
            LOCALADMINPASS = $adminPass
            JOINDOMAIN     = ''
            DOMAINOU       = ''
            DOMAINUSER     = ''
            DOMAINPASS     = ''
            SERIAL         = $identity.SanitizedSerial
        }
    }
    $plan
}

# --- policy "blocked" remediation screen -----------------------------------
function Show-PolicyBlocked {
    param([pscustomobject]$Inventory, [pscustomobject]$Decision)
    $ui.PolicyTitle.Text = if ($Decision.Action -eq 'hold') { 'Deployment on hold' } else { 'Deployment blocked by policy' }
    $ui.PolicySummary.Text = Format-InventorySummary -Inventory $Inventory
    $ui.PolicyReasons.Children.Clear(); $ui.PolicyRemediations.Children.Clear()

    $add = {
        param($panel, $text, $color)
        $tb = New-Object System.Windows.Controls.TextBlock
        $tb.Text = "- $text"; $tb.TextWrapping = 'Wrap'; $tb.FontSize = 12
        $tb.Foreground = $color; $tb.Margin = '0,0,0,4'
        $panel.Children.Add($tb) | Out-Null
    }
    if ($Decision.Reasons)      { foreach ($r in $Decision.Reasons)      { & $add $ui.PolicyReasons $r '#F2B8B8' } }
    else                        { & $add $ui.PolicyReasons 'No specific reasons returned.' '#AEBFD4' }
    if ($Decision.Remediations) { foreach ($m in $Decision.Remediations) { & $add $ui.PolicyRemediations $m '#EBEFF5' } }
    else                        { & $add $ui.PolicyRemediations 'Contact the deployment administrator.' '#AEBFD4' }

    Show-Panel 'PolicyPanel'
    $ui.StatusBar.Text = "Policy: $($Decision.Action.ToUpper()) - $(@($Decision.Reasons).Count) check(s) failed."
}

# --- startup auto-route ----------------------------------------------------
# Phase 1: collect inventory. Phase 2: OPA hard gate. Phase 3: route (server
# per-machine config -> zero-touch; otherwise interactive). A non-allow decision
# stops here and shows remediations -- no disk is ever touched.
function Start-AutoRoute {
    Show-Panel 'ProgressPanel'
    $ui.StageText.Foreground = '#FFFFFF'
    $ui.Bar.Value = 0; $ui.PercentText.Text = ''
    $ui.LogText.Text = ''; $ui.BtnFinish.Visibility = 'Collapsed'

    # Phase 1 - inventory
    $ui.StageText.Text = "Collecting hardware inventory..."
    $ui.StatusBar.Text = "Reading make/model/serial/BIOS/TPM ..."
    $win.Dispatcher.Invoke([action]{}, [System.Windows.Threading.DispatcherPriority]::Render)
    $inv = Get-Inventory
    $script:inventory = $inv
    Save-Inventory -Inventory $inv | Out-Null
    Add-Log "Inventory collected:"; Add-Log (Format-InventorySummary -Inventory $inv)

    # Phase 2 - policy hard gate
    $settings = Get-ZtpSettings
    $ui.StageText.Text = "Evaluating deployment policy..."
    $ui.StatusBar.Text = "Submitting inventory to the policy engine ..."
    $win.Dispatcher.Invoke([action]{}, [System.Windows.Threading.DispatcherPriority]::Render)
    $decision = Invoke-PolicyEvaluation -PolicyUrl $settings.policyUrl -Inventory $inv `
                    -FailAction ($(if ($settings.PSObject.Properties.Name -contains 'policyFailAction') { $settings.policyFailAction } else { 'hold' }))
    Add-Log "Policy decision: $($decision.Action.ToUpper())"
    Send-ZtpStatus -ServerUrl $script:apiUrl -Identity $identity `
        -State ($(if ($decision.Allow) { 'progress' } else { 'failed' })) `
        -Message "Policy $($decision.Action): $(@($decision.Reasons) -join '; ')"

    if (-not $decision.Allow) {
        Show-PolicyBlocked -Inventory $inv -Decision $decision
        return
    }

    # Phase 3 - config probe + route
    $ui.StageText.Text = "Contacting deploy server for this machine..."
    $ui.StatusBar.Text = "Checking server for $($identity.SanitizedSerial).json ..."
    $win.Dispatcher.Invoke([action]{}, [System.Windows.Threading.DispatcherPriority]::Render)

    $cfg = $null
    try { $cfg = Get-ZtpConfig -Quiet -Inventory $inv } catch { $cfg = $null }
    if ($cfg -and $decision.Config) { $cfg = Merge-PolicyConfig -Config $cfg -PolicyConfig $decision.Config }

    if ($cfg -and $cfg.HasMachineConfig -and $cfg.Mode -ne 'interactive') {
        Start-ZeroTouch -Config $cfg
    } else {
        Load-Disks
        $ui.NameBox.Text = "WKS-$($identity.SanitizedSerial)"
        Show-Panel 'ConfigPanel'
        $ui.StatusBar.Text = if ($cfg -and $cfg.HasMachineConfig) {
            "Server config requests interactive mode."
        } else {
            "Policy passed. No server config for serial $($identity.Serial) - interactive mode."
        }
    }
}

# --- zero-touch flow -------------------------------------------------------
function Start-ZeroTouch {
    param($Config)   # optional pre-fetched config (from Start-AutoRoute); else fetched here
    Show-Panel 'ProgressPanel'
    $ui.StageText.Foreground = '#FFFFFF'
    $ui.StageText.Text = "Contacting deploy server..."
    Add-Log "Resolving zero-touch config for serial $($identity.Serial)..."
    $cfg = $Config
    if (-not $cfg) {
        try {
            $cfg = Get-ZtpConfig -Quiet
        } catch {
            Add-Log "[ERROR] Could not get config: $($_.Exception.Message)"
            $ui.StageText.Text = "Config fetch failed"; return
        }
    }
    Add-Log "Config source: $($cfg.Source)"
    Add-Log "Disk rule: $($cfg.DiskRule)   Name: $($cfg.ComputerName)"

    $disk = Resolve-TargetDisk -Rule $cfg.DiskRule
    Add-Log "Resolved target: Disk $($disk.Index) ($($disk.SizeGB) GB, $($disk.Model))"

    $plan = @{
        DiskIndex        = $disk.Index
        ComputerName     = $cfg.ComputerName
        ImageUrl         = $cfg.ImageUrl
        UnattendTemplate = Join-Path $ScriptRoot 'unattend.template.xml'
        UnattendValues   = $cfg.Unattend
    }

    # Safety countdown so an operator can abort a wipe.
    $secs = 10
    $ui.StageText.Text = "Zero-touch deploy to Disk $($disk.Index) in $secs s (close window to abort)"
    $cd = New-Object System.Windows.Threading.DispatcherTimer
    $cd.Interval = [TimeSpan]::FromSeconds(1)
    $cd.Add_Tick({
        $script:secs--
        if ($script:secs -le 0) {
            $cd.Stop()
            Start-DeploymentWorker -Plan $plan -ServerUrl $cfg.ApiUrl
        } else {
            $ui.StageText.Text = "Zero-touch deploy to Disk $($disk.Index) in $script:secs s (close window to abort)"
        }
    })
    $cd.Start()
}

# --- event wiring ----------------------------------------------------------
$ui.BtnInteractive.Add_Click({ Load-Disks; Show-Panel 'ConfigPanel'
                               $ui.NameBox.Text = "WKS-$($identity.SanitizedSerial)" })
$ui.BtnZeroTouch.Add_Click({ Start-ZeroTouch })
$ui.BtnBack.Add_Click({ Show-Panel 'ModePanel' })
$ui.BtnQuit.Add_Click({ $win.Close() })
$ui.BtnRecheck.Add_Click({ Start-AutoRoute })     # re-collect + re-evaluate policy
$ui.BtnPolicyExit.Add_Click({ $win.Close() })

$ui.BtnDeploy.Add_Click({
    if (($ui.ConfirmBox.Text).Trim().ToUpper() -ne 'ERASE') {
        $ui.StatusBar.Text = "Type ERASE to confirm."; return
    }
    try {
        $plan = New-InteractivePlan
        Start-DeploymentWorker -Plan $plan -ServerUrl $script:apiUrl   # interactive now reports too
    } catch {
        $ui.StatusBar.Text = $_.Exception.Message
    }
})

$ui.BtnFinish.Add_Click({
    if ($ui.BtnFinish.Content -like 'Reboot*') {
        if ($script:rebootTimer) { $script:rebootTimer.Stop() }
        Start-Process 'wpeutil.exe' -ArgumentList 'reboot'
    } else { $win.Close() }
})

# Open on a "contacting server" splash; auto-route picks zero-touch vs interactive
# on first render. The mode picker remains reachable via the config panel's Back button.
$ui.StageText.Text = "Contacting deploy server for this machine..."
Show-Panel 'ProgressPanel'
$script:autoRouted = $false
$win.Add_ContentRendered({ if (-not $script:autoRouted) { $script:autoRouted = $true; Start-AutoRoute } })
[void]$win.ShowDialog()

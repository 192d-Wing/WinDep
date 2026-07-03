<#
    Invoke-Policy.ps1  —  submit inventory to Open Policy Agent (OPA) and normalize
    the decision into a hard gate.

    OPA REST: POST <policyUrl> with body { "input": <inventory> }; OPA replies
    { "result": <decision> }. The Rego policy (Server/policy/windep.rego) is authored
    to emit a decision document:
        { "action": "allow"|"deny"|"hold", "allow": bool,
          "reasons": [..], "remediations": [..], "config": {..}? }

    Fail-closed: if OPA is unreachable or returns no decision, this returns the
    configured fail action (default 'hold') so no disk is wiped without an explicit
    allow. Set policyUrl empty in ztp.config.json to disable the gate (returns allow).

    HTTPS validates against the internal root CA baked into WinPE.
#>

Set-StrictMode -Version Latest
try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch { }

function New-PolicyDecision {
    param([string]$Action, [string[]]$Reasons = @(), [string[]]$Remediations = @(), $Config = $null)
    [pscustomobject]@{
        Action       = $Action
        Allow        = ($Action -eq 'allow')
        Reasons      = @($Reasons)
        Remediations = @($Remediations)
        Config       = $Config
    }
}

function Invoke-PolicyEvaluation {
    <#
      .PARAMETER PolicyUrl  OPA data endpoint, e.g. https://opa.jhics.org/v1/data/windep/decision
      .PARAMETER Inventory  object from Get-Inventory
      .PARAMETER FailAction 'hold' | 'deny' — decision returned when OPA can't be reached
    #>
    param(
        [string]$PolicyUrl,
        [Parameter(Mandatory)][pscustomobject]$Inventory,
        [ValidateSet('hold','deny')][string]$FailAction = 'hold'
    )

    if ([string]::IsNullOrWhiteSpace($PolicyUrl)) {
        return New-PolicyDecision -Action 'allow' -Reasons @('Policy engine not configured (gate disabled).')
    }

    Add-Type -AssemblyName System.Net.Http -ErrorAction SilentlyContinue
    $client = New-Object System.Net.Http.HttpClient
    $client.Timeout = [TimeSpan]::FromSeconds(30)
    try {
        $body = @{ input = $Inventory } | ConvertTo-Json -Depth 12 -Compress
        $content = New-Object System.Net.Http.StringContent($body, [System.Text.Encoding]::UTF8, 'application/json')
        $resp = $client.PostAsync($PolicyUrl, $content).GetAwaiter().GetResult()
        if (-not $resp.IsSuccessStatusCode) {
            return New-PolicyDecision -Action $FailAction `
                -Reasons @("Policy engine returned HTTP $([int]$resp.StatusCode).") `
                -Remediations @('Verify the OPA endpoint, policy path, and TLS certificate.')
        }
        $json = $resp.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        $parsed = $json | ConvertFrom-Json

        # OPA wraps the decision in "result"; an undefined path yields no result key.
        $result = if ($parsed.PSObject.Properties.Name -contains 'result') { $parsed.result } else { $null }
        if (-not $result -or -not ($result.PSObject.Properties.Name -contains 'action')) {
            return New-PolicyDecision -Action $FailAction `
                -Reasons @('Policy engine returned no decision (undefined result).') `
                -Remediations @('Confirm the Rego package path matches policyUrl and defines "decision".')
        }

        $action = "$($result.action)".ToLower()
        if ($action -notin @('allow','deny','hold')) { $action = 'deny' }
        $reasons      = if ($result.PSObject.Properties.Name -contains 'reasons')      { @($result.reasons) }      else { @() }
        $remediations = if ($result.PSObject.Properties.Name -contains 'remediations') { @($result.remediations) } else { @() }
        $config       = if ($result.PSObject.Properties.Name -contains 'config')       { $result.config }          else { $null }

        return New-PolicyDecision -Action $action -Reasons $reasons -Remediations $remediations -Config $config
    }
    catch {
        return New-PolicyDecision -Action $FailAction `
            -Reasons @("Policy engine unreachable: $($_.Exception.Message)") `
            -Remediations @('Check network/DNS and that OPA is serving over HTTPS with a trusted certificate.')
    }
    finally { $client.Dispose() }
}

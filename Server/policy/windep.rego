# WinDep deployment policy (Open Policy Agent / Rego)
#
# Evaluated at boot in WinPE: the agent POSTs the collected inventory as `input`
# to  POST /v1/data/windep/decision  and reads `.result`.
#
# Emits a hard-gate decision document:
#   { "action": "allow"|"deny"|"hold", "allow": bool,
#     "reasons": [..], "remediations": [..] }
#
# Requires OPA >= 0.59 (import rego.v1). Adjust the requirement tables below to
# your fleet. BIOS comparison uses semver.compare — see the README for the caveat
# on non-semver BIOS strings.

package windep

import rego.v1

# ------------------------------------------------------------------ requirements
allowed_models := {
	"OptiPlex 7010",
	"OptiPlex 7020",
	"Latitude 5440",
	"Latitude 7450",
	"Dell Pro Max 16 MC16250",
}

min_ram_gb := 8

min_disk_gb := 240

# Minimum SMBIOS BIOS version per model.
min_bios := {
	"OptiPlex 7010": "1.28.0",
	"OptiPlex 7020": "1.6.0",
	"Latitude 5440": "1.15.0",
	"Latitude 7450": "1.4.0",
	"Dell Pro Max 16 MC16250": "1.4.0",
}

# ------------------------------------------------------------------- violations
# Each violation carries a human reason and (optionally) a remediation step.

violations contains v if {
	input.firmware.firmwareType != "UEFI"
	v := {
		"reason": "Firmware is not booted in UEFI mode",
		"remediation": "Enable UEFI and disable Legacy/CSM in firmware setup",
	}
}

violations contains v if {
	input.firmware.secureBoot != "on"
	v := {
		"reason": sprintf("Secure Boot is %v", [input.firmware.secureBoot]),
		"remediation": "Enable Secure Boot in firmware setup",
	}
}

violations contains v if {
	not input.security.tpmPresent
	v := {
		"reason": "No TPM detected",
		"remediation": "Enable the TPM / Intel PTT / AMD fTPM in firmware setup",
	}
}

violations contains v if {
	input.security.tpmPresent
	input.security.tpmVersion != "2.0"
	v := {
		"reason": sprintf("TPM %v present; 2.0 required", [input.security.tpmVersion]),
		"remediation": "Provision or upgrade to TPM 2.0",
	}
}

violations contains v if {
	input.memory.totalGB < min_ram_gb
	v := {
		"reason": sprintf("%vGB RAM installed; %vGB required", [input.memory.totalGB, min_ram_gb]),
		"remediation": sprintf("Install at least %vGB RAM", [min_ram_gb]),
	}
}

violations contains v if {
	every d in input.storage.disks {
		d.sizeGB < min_disk_gb
	}
	v := {
		"reason": sprintf("No disk of at least %vGB found", [min_disk_gb]),
		"remediation": sprintf("Install a disk of at least %vGB", [min_disk_gb]),
	}
}

violations contains v if {
	not allowed_models[input.system.model]
	v := {
		"reason": sprintf("Model %q is not in the approved list", [input.system.model]),
		"remediation": "Use an approved hardware model, or add it to allowed_models",
	}
}

violations contains v if {
	req := min_bios[input.system.model]
	semver.compare(input.firmware.biosVersion, req) < 0
	v := {
		"reason": sprintf("BIOS %v is below the required %v for %v", [input.firmware.biosVersion, req, input.system.model]),
		"remediation": sprintf("Update BIOS to %v or newer", [req]),
	}
}

# --------------------------------------------------------------------- decision
reasons := sort([v.reason | some v in violations])

remediations := sort([v.remediation | some v in violations; v.remediation != ""])

# Fail-closed default (only reached if `violations` itself fails to evaluate).
default decision := {
	"action": "deny",
	"allow": false,
	"reasons": ["Policy did not evaluate (fail-closed)"],
	"remediations": [],
}

decision := {
	"action": "allow",
	"allow": true,
	"reasons": [],
	"remediations": [],
} if {
	count(violations) == 0
}

decision := {
	"action": "deny",
	"allow": false,
	"reasons": reasons,
	"remediations": remediations,
} if {
	count(violations) > 0
}

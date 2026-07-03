<#
    Get-Inventory.ps1  —  collect hardware/firmware inventory in WinPE.

    Emits an object conforming to Schema/inventory.schema.json. Best-effort: every
    probe is wrapped so a missing WMI provider degrades to null/'unknown' rather than
    throwing. The result is the `input` document handed to the OPA policy engine.

    Dot-source this file, then call Get-Inventory. Save-Inventory writes it to
    X:\Windows\Temp\inventory.json for post-mortem.

    VALIDATION CHECKLIST (WinPE VM + real hardware):
      [ ] system.model/serial populated on real hardware and a VM (isVirtual=true)
      [ ] firmware.firmwareType = UEFI and secureBoot reflects the VM/HW setting
      [ ] security.tpmPresent/tpmVersion correct where a TPM exists
      [ ] storage.disks mediaType classifies NVMe/SSD/HDD
      [ ] output validates against Schema/inventory.schema.json
#>

Set-StrictMode -Version Latest
try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch { }

function Get-ChassisTypeName {
    param([int]$Code)
    switch ($Code) {
        3 { 'Desktop' } 4 { 'Low Profile Desktop' } 5 { 'Pizza Box' } 6 { 'Mini Tower' }
        7 { 'Tower' } 8 { 'Portable' } 9 { 'Laptop' } 10 { 'Notebook' } 11 { 'Handheld' }
        12 { 'Docking Station' } 13 { 'All-in-One' } 14 { 'Sub Notebook' } 15 { 'Space-Saving' }
        18 { 'Expansion Chassis' } 21 { 'Peripheral' } 23 { 'Rack Mount' } 24 { 'Sealed-Case PC' }
        30 { 'Tablet' } 31 { 'Convertible' } 32 { 'Detachable' } default { 'Unknown' }
    }
}

function Get-Inventory {
    [CmdletBinding()] param([string]$BootMethod = 'unknown')

    $bios = Get-CimInstance Win32_BIOS -ErrorAction SilentlyContinue
    $cs   = Get-CimInstance Win32_ComputerSystem -ErrorAction SilentlyContinue
    $csp  = Get-CimInstance Win32_ComputerSystemProduct -ErrorAction SilentlyContinue
    $enc  = Get-CimInstance Win32_SystemEnclosure -ErrorAction SilentlyContinue | Select-Object -First 1

    # ---- system ----
    $manufacturer = if ($cs) { "$($cs.Manufacturer)".Trim() } else { $null }
    $model        = if ($cs) { "$($cs.Model)".Trim() } else { $null }
    $serial       = if ($bios -and $bios.SerialNumber) { "$($bios.SerialNumber)".Trim() } else { $null }
    if (-not $serial -or $serial -match '^(0+|To be filled.*|Default string|System Serial Number|None)$') {
        if ($csp) { $serial = "$($csp.IdentifyingNumber)".Trim() }
    }
    $chassisCode = if ($enc -and $enc.ChassisTypes) { [int]($enc.ChassisTypes | Select-Object -First 1) } else { 0 }
    $isVirtual = ($manufacturer -match 'VMware|VirtualBox|innotek|QEMU|Xen|Parallels|Microsoft Corporation' -and
                  $model -match 'Virtual|VMware|VirtualBox|KVM|Hyper-V') -or
                 ($model -match 'Virtual Machine')
    $chassis = if ($isVirtual) { 'Virtual' } else { Get-ChassisTypeName $chassisCode }

    # ---- firmware ----
    $fwType = 'unknown'
    try {
        $pe = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control' -Name PEFirmwareType -ErrorAction Stop).PEFirmwareType
        $fwType = if ($pe -eq 2) { 'UEFI' } elseif ($pe -eq 1) { 'BIOS' } else { 'unknown' }
    } catch { }

    $secureBoot = 'unknown'
    try {
        $sb = Confirm-SecureBootUEFI -ErrorAction Stop
        $secureBoot = if ($sb) { 'on' } else { 'off' }
    } catch {
        try {
            $v = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\SecureBoot\State' -Name UEFISecureBootEnabled -ErrorAction Stop).UEFISecureBootEnabled
            $secureBoot = if ($v -eq 1) { 'on' } elseif ($v -eq 0) { 'off' } else { 'unknown' }
        } catch { }
    }

    $biosDate = $null
    if ($bios -and $bios.ReleaseDate) { try { $biosDate = ([datetime]$bios.ReleaseDate).ToString('o') } catch { } }

    # ---- security / TPM ----
    $tpmPresent = $false; $tpmVersion = $null; $tpmEnabled = $null; $tpmActivated = $null
    try {
        $tpm = Get-CimInstance -Namespace 'root\cimv2\Security\MicrosoftTpm' -ClassName Win32_Tpm -ErrorAction Stop
        if ($tpm) {
            $tpmPresent   = $true
            $tpmEnabled   = [bool]$tpm.IsEnabled_InitialValue
            $tpmActivated = [bool]$tpm.IsActivated_InitialValue
            if ($tpm.SpecVersion) { $tpmVersion = (("$($tpm.SpecVersion)" -split ',')[0]).Trim() }
        }
    } catch { }

    # ---- cpu ----
    $cpu = Get-CimInstance Win32_Processor -ErrorAction SilentlyContinue | Select-Object -First 1
    $archMap = @{ 0='x86'; 1='MIPS'; 2='Alpha'; 3='PowerPC'; 5='ARM'; 6='ia64'; 9='x64'; 12='ARM64' }
    $arch = if ($cpu) { $archMap[[int]$cpu.Architecture] } else { $null }; if (-not $arch) { $arch = 'unknown' }

    # ---- memory ----
    $mods = @(Get-CimInstance Win32_PhysicalMemory -ErrorAction SilentlyContinue)
    $totalGB = if ($cs -and $cs.TotalPhysicalMemory) { [math]::Round($cs.TotalPhysicalMemory/1GB, 1) }
               elseif ($mods) { [math]::Round((($mods | Measure-Object Capacity -Sum).Sum)/1GB, 1) } else { 0 }
    $moduleList = foreach ($m in $mods) {
        [pscustomobject]@{
            sizeGB       = [math]::Round($m.Capacity/1GB, 1)
            speedMHz     = if ($m.Speed) { [int]$m.Speed } else { $null }
            manufacturer = if ($m.Manufacturer) { "$($m.Manufacturer)".Trim() } else { $null }
            partNumber   = if ($m.PartNumber) { "$($m.PartNumber)".Trim() } else { $null }
        }
    }

    # ---- storage (prefer MSFT_PhysicalDisk for media/bus type) ----
    $disks = @()
    try {
        $pd = Get-CimInstance -Namespace 'root\Microsoft\Windows\Storage' -ClassName MSFT_PhysicalDisk -ErrorAction Stop
        $mediaMap = @{ 0='unknown'; 3='HDD'; 4='SSD'; 5='SCM' }
        $busMap = @{ 1='SCSI'; 2='ATAPI'; 3='ATA'; 4='1394'; 5='SSA'; 6='FibreChannel'; 7='USB';
                     8='RAID'; 9='iSCSI'; 10='SAS'; 11='SATA'; 12='SD'; 13='MMC'; 17='NVMe'; 18='SCM' }
        $disks = foreach ($d in ($pd | Sort-Object DeviceId)) {
            $media = $mediaMap[[int]$d.MediaType]; if (-not $media) { $media = 'unknown' }
            $bus   = $busMap[[int]$d.BusType]
            if ([int]$d.BusType -eq 17) { $media = 'NVMe' }
            [pscustomobject]@{
                index     = [int]$d.DeviceId
                model     = "$($d.FriendlyName)".Trim()
                sizeGB    = [math]::Round($d.Size/1GB, 0)
                mediaType = $media
                bus       = $bus
                serial    = if ($d.SerialNumber) { "$($d.SerialNumber)".Trim() } else { $null }
            }
        }
    } catch { }
    if (-not $disks) {
        $disks = foreach ($d in (Get-CimInstance Win32_DiskDrive -ErrorAction SilentlyContinue | Sort-Object Index)) {
            [pscustomobject]@{
                index     = [int]$d.Index
                model     = "$($d.Model)".Trim()
                sizeGB    = [math]::Round($d.Size/1GB, 0)
                mediaType = if ($d.MediaType -match 'Removable') { 'removable' } else { 'unknown' }
                bus       = $d.InterfaceType
                serial    = if ($d.SerialNumber) { "$($d.SerialNumber)".Trim() } else { $null }
            }
        }
    }

    # ---- network ----
    $adapters = foreach ($n in (Get-CimInstance Win32_NetworkAdapter -ErrorAction SilentlyContinue |
                                Where-Object { $_.PhysicalAdapter -and $_.MACAddress })) {
        $nicModel = if ($n.ProductName) { "$($n.ProductName)".Trim() }
                    elseif ($n.Description) { "$($n.Description)".Trim() } else { "$($n.Name)".Trim() }
        [pscustomobject]@{
            name  = "$($n.Name)".Trim()
            make  = if ($n.Manufacturer) { "$($n.Manufacturer)".Trim() } else { $null }
            model = $nicModel
            mac   = $n.MACAddress
            type  = $n.AdapterType
        }
    }

    # ---- gpu ----
    $gpu = foreach ($g in (Get-CimInstance Win32_VideoController -ErrorAction SilentlyContinue)) {
        $gpuModel = if ($g.VideoProcessor) { "$($g.VideoProcessor)".Trim() } else { "$($g.Name)".Trim() }
        [pscustomobject]@{
            name          = "$($g.Name)".Trim()
            make          = if ($g.AdapterCompatibility) { "$($g.AdapterCompatibility)".Trim() } else { $null }
            model         = $gpuModel
            driverVersion = $g.DriverVersion
        }
    }

    [pscustomobject]@{
        schemaVersion = '1.0'
        collectedAt   = (Get-Date).ToUniversalTime().ToString('o')
        agent = [pscustomobject]@{ version = '1.0'; bootMethod = $BootMethod; peArch = $env:PROCESSOR_ARCHITECTURE }
        system = [pscustomobject]@{
            manufacturer = $manufacturer; model = $model
            sku = if ($csp) { $csp.Version } else { $null }
            family = if ($cs) { $cs.SystemFamily } else { $null }
            serial = if ($serial) { $serial } else { 'UNKNOWN' }
            uuid = if ($csp) { $csp.UUID } else { $null }
            assetTag = if ($enc -and $enc.SMBIOSAssetTag) { "$($enc.SMBIOSAssetTag)".Trim() } else { $null }
            chassisType = $chassis; isVirtual = [bool]$isVirtual
        }
        firmware = [pscustomobject]@{
            firmwareType = $fwType
            biosVendor = if ($bios) { "$($bios.Manufacturer)".Trim() } else { $null }
            biosVersion = if ($bios -and $bios.SMBIOSBIOSVersion) { "$($bios.SMBIOSBIOSVersion)".Trim() } else { 'unknown' }
            biosReleaseDate = $biosDate
            smbiosVersion = if ($bios) { "$($bios.SMBIOSMajorVersion).$($bios.SMBIOSMinorVersion)" } else { $null }
            secureBoot = $secureBoot
        }
        security = [pscustomobject]@{
            tpmPresent = [bool]$tpmPresent; tpmVersion = $tpmVersion
            tpmEnabled = $tpmEnabled; tpmActivated = $tpmActivated
        }
        cpu = [pscustomobject]@{
            name = if ($cpu) { "$($cpu.Name)".Trim() } else { $null }
            manufacturer = if ($cpu) { "$($cpu.Manufacturer)".Trim() } else { $null }
            cores = if ($cpu) { [int]$cpu.NumberOfCores } else { 0 }
            logicalProcessors = if ($cpu) { [int]$cpu.NumberOfLogicalProcessors } else { 0 }
            architecture = $arch
            maxClockMHz = if ($cpu) { [int]$cpu.MaxClockSpeed } else { $null }
        }
        memory = [pscustomobject]@{
            totalGB = $totalGB; moduleCount = @($mods).Count; modules = @($moduleList)
        }
        storage = [pscustomobject]@{ disks = @($disks) }
        network = [pscustomobject]@{ adapters = @($adapters) }
        gpu = @($gpu)
    }
}

function Save-Inventory {
    param([Parameter(Mandatory)][pscustomobject]$Inventory,
          [string]$Path = 'X:\Windows\Temp\inventory.json')
    try {
        $Inventory | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $Path -Encoding UTF8
    } catch { Write-Warning "Could not save inventory: $($_.Exception.Message)" }
    $Path
}

function Format-InventorySummary {
    param([Parameter(Mandatory)][pscustomobject]$Inventory)
    $i = $Inventory
    @(
        "Make/Model : $($i.system.manufacturer) $($i.system.model)  [$($i.system.chassisType)]"
        "Serial     : $($i.system.serial)"
        "Firmware   : $($i.firmware.firmwareType)  BIOS $($i.firmware.biosVersion)  SecureBoot=$($i.firmware.secureBoot)"
        "TPM        : present=$($i.security.tpmPresent) version=$($i.security.tpmVersion)"
        "CPU / RAM  : $($i.cpu.name) / $($i.memory.totalGB) GB"
        "Disks      : " + (($i.storage.disks | ForEach-Object { "$($_.sizeGB)GB $($_.mediaType)" }) -join ', ')
    ) -join "`n"
}

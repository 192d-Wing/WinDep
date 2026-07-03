@echo off
setlocal enableextensions
:: %1 = data drive letter passed in by startnet.cmd (e.g. "D")
set DATA=%1
if "%DATA%"=="" set DATA=%~d0
set DATA=%DATA::=%

cls
echo ============================================================
echo    Windows 11 Pro - Simple Deployment
echo ============================================================
echo.

if not exist "%DATA%:\install.wim" (
  echo [!] install.wim not found at %DATA%:\install.wim
  echo     Cannot continue.
  goto :end
)
echo  Image source : %DATA%:\install.wim
echo.

:pickdisk
echo Scanning for disks...
echo.
echo list disk > "%TEMP%\ld.txt"
diskpart /s "%TEMP%\ld.txt"
echo.
echo ------------------------------------------------------------
echo  WARNING: the disk you choose will be COMPLETELY ERASED.
echo ------------------------------------------------------------
set "DISKNUM="
set /p DISKNUM=Enter the DISK NUMBER to install Windows to (or Q to quit):
if /I "%DISKNUM%"=="Q" goto :end
if "%DISKNUM%"=="" goto :pickdisk

echo.
set "CONF="
set /p CONF=Type ERASE to wipe disk %DISKNUM% and install Windows:
if /I not "%CONF%"=="ERASE" (
  echo Cancelled.
  echo.
  goto :pickdisk
)

:: --- build diskpart script: GPT = EFI(260 FAT32) + MSR(16) + Windows(NTFS rest) ---
set "DP=%TEMP%\deploy_dp.txt"
> "%DP%"  echo select disk %DISKNUM%
>> "%DP%" echo clean
>> "%DP%" echo convert gpt
>> "%DP%" echo create partition efi size=260
>> "%DP%" echo format quick fs=fat32 label="System"
>> "%DP%" echo assign letter=S
>> "%DP%" echo create partition msr size=16
>> "%DP%" echo create partition primary
>> "%DP%" echo format quick fs=ntfs label="Windows"
>> "%DP%" echo assign letter=W
>> "%DP%" echo exit

echo.
echo Partitioning disk %DISKNUM% ...
diskpart /s "%DP%"
if errorlevel 1 (
  echo [!] diskpart failed. Aborting.
  goto :end
)

echo.
echo Applying Windows 11 Pro image to W:\ ...
echo (this can take several minutes)
dism /Apply-Image /ImageFile:"%DATA%:\install.wim" /Index:1 /ApplyDir:W:\
if errorlevel 1 (
  echo [!] DISM apply failed. Aborting.
  goto :end
)

echo.
echo Writing UEFI boot files to the EFI System partition ...
bcdboot W:\Windows /s S: /f UEFI
if errorlevel 1 (
  echo [!] bcdboot failed. Aborting.
  goto :end
)

echo.
echo ============================================================
echo    Deployment complete on disk %DISKNUM%.
echo ============================================================
echo  Remove the USB drive, then press any key to reboot into
echo  Windows out-of-box setup.
echo.
pause
wpeutil reboot
goto :eof

:end
echo.
echo Press any key to return to the command prompt.
pause

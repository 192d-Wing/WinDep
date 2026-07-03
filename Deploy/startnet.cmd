@echo off
:: startnet.cmd  —  WinPE bootstrap for WinDep.
:: Copied to <mount>\Windows\System32\startnet.cmd by Build-WinPE.ps1 and runs
:: automatically when WinPE finishes booting.

:: 1) Bring up networking (needed for HTTPS config/image pull).
wpeinit

:: 2) High-perf power + keep the screen awake during long applies.
powercfg /s SCHEME_MIN >nul 2>&1

:: 3) Locate the Deploy payload. It is injected at X:\Deploy by the build script,
::    but also probe removable drives in case it is run from a USB layout.
set "DEPLOY=X:\Deploy"
if exist "%DEPLOY%\DeployUI.ps1" goto :launch
for %%D in (C D E F G H I J K L) do (
  if exist "%%D:\Deploy\DeployUI.ps1" set "DEPLOY=%%D:\Deploy" & goto :launch
)

:launch
echo.
echo  Starting Windows 11 Deployment UI...
echo  (payload: %DEPLOY%)
echo.

:: 3b) Trust the internal root CA so in-WinPE HTTPS validates against it.
::     Uses .NET X509Store (WinPE-NetFx) — reliable without certutil.
if exist "%DEPLOY%\InternalRootCA.cer" (
  echo  Importing internal root CA into the WinPE trust store...
  powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$c=New-Object System.Security.Cryptography.X509Certificates.X509Certificate2('%DEPLOY%\InternalRootCA.cer'); $s=New-Object System.Security.Cryptography.X509Certificates.X509Store('Root','LocalMachine'); $s.Open('ReadWrite'); $s.Add($c); $s.Close()"
)

:: 4) Launch the WPF UI. If PowerShell/WPF fails to load, fall back to the
::    text-mode deploy.cmd so the operator is never stranded.
powershell.exe -NoProfile -ExecutionPolicy Bypass -STA -File "%DEPLOY%\DeployUI.ps1"
if errorlevel 1 (
  echo.
  echo  [!] The graphical UI could not start. Falling back to text-mode deploy.
  echo.
  if exist "%DEPLOY%\deploy.cmd" (
    call "%DEPLOY%\deploy.cmd"
  ) else (
    echo  deploy.cmd not found next to the UI. Dropping to command prompt.
  )
)

:: 5) If everything exits, leave a usable prompt rather than rebooting blindly.
cmd.exe

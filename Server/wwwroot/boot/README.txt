This folder is populated by Build\Build-WinPE.ps1 (the -StageDir target).

After a build it contains:
  bootmgfw.efi        MS-signed boot loader (Secure Boot)
  Boot\BCD            ramdisk BCD -> \sources\boot.wim
  Boot\boot.sdi
  sources\boot.wim    the WinDep WinPE image

Serve this folder over HTTPS (UEFI HTTPS Boot) and/or from a TFTP root (PXE fallback).
See ..\..\README.md (Server/README.md) for DHCP vendor-class wiring.

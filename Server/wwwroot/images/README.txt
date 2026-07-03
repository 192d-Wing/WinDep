Place install.wim here for NETWORK imaging (served over HTTPS as
  https://<server>/images/install.wim

The WinPE agent uses a USB-local install.wim when present; otherwise it downloads
this file over HTTPS onto the freshly-partitioned target volume, then applies it.

This directory is intentionally empty in the repo (the WIM is ~5.7 GB).

# SteamOS build and deploy tooling

Helper scripts for running this fork on SteamOS (read-only root, no C toolchain, and atomic
updates that wipe custom files in /etc).

- `build-openlinkhub.sh <src> <out>`: builds inside a `golang:<version>-bookworm` podman
  container (cgo needs libudev and libpipewire headers that SteamOS doesn't ship).
- `deploy-openlinkhub.sh <binary> <src>`: installs to /opt/OpenLinkHub, keeping `.prev`
  copies of the binary, web/, static/ and reference data. User profiles, RGB settings and
  config are never touched. `--rollback` restores the previous deploy.
- `restore-after-steamos-update.sh`: run with sudo after a SteamOS update. It restores the
  udev rules, the nct6775 module load and the corsair_psu blacklist, and writes an
  atomic-update keep-list so future updates preserve them.

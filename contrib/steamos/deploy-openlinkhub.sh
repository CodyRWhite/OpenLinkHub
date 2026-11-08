#!/bin/bash
# Deploy an OpenLinkHub build to /opt/OpenLinkHub, keeping the previous binary for rollback.
# Usage: deploy-openlinkhub.sh <binary> <source-dir>     (also copies web/*.html, static/ and reference data in database/; never user profiles)
#        deploy-openlinkhub.sh --rollback
set -euo pipefail
O=/opt/OpenLinkHub
if [ "${1:-}" = "--rollback" ]; then
  sudo systemctl stop OpenLinkHub
  sudo cp -a $O/OpenLinkHub.prev $O/OpenLinkHub
  [ -d $O/web.prev ] && sudo rm -rf $O/web && sudo cp -a $O/web.prev $O/web
  [ -d $O/static.prev ] && sudo rm -rf $O/static && sudo cp -a $O/static.prev $O/static
  for p in $O/database/*.prev; do [ -d "$p" ] && sudo rm -rf "${p%.prev}" && sudo cp -a "$p" "${p%.prev}"; done
  sudo systemctl start OpenLinkHub; echo "Rolled back."; exit 0
fi
BIN=$1; SRC=$2
sudo systemctl stop OpenLinkHub
sudo cp -a $O/OpenLinkHub $O/OpenLinkHub.prev
sudo rm -rf $O/web.prev $O/static.prev && sudo cp -a $O/web $O/web.prev && sudo cp -a $O/static $O/static.prev
sudo install -o openlinkhub -g openlinkhub -m 755 "$BIN" $O/OpenLinkHub
sudo install -o openlinkhub -g openlinkhub -m 644 "$SRC"/web/*.html $O/web/
sudo cp -r "$SRC"/static/. $O/static/ && sudo chown -R openlinkhub:openlinkhub $O/static
# Reference data shipped with the source (never user data such as profiles/, rgb/,
# scheduler.json, temperatures/ or dashboard.json, which stay as they are).
for dir in applications apps external keyboard language motherboard xeneon; do
  [ -d "$SRC/database/$dir" ] || continue
  sudo mkdir -p $O/database/$dir
  [ -d $O/database/$dir ] && sudo cp -a $O/database/$dir $O/database/$dir.prev.tmp && sudo rm -rf $O/database/$dir.prev && sudo mv $O/database/$dir.prev.tmp $O/database/$dir.prev
  sudo cp -r "$SRC/database/$dir"/. $O/database/$dir/
done
sudo chown -R openlinkhub:openlinkhub $O/database
sudo systemctl start OpenLinkHub
sleep 5
systemctl is-active OpenLinkHub

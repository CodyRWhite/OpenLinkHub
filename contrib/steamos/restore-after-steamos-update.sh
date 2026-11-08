#!/bin/bash
# Restore the Fans-and-LEDs system config that a SteamOS atomic update wipes from /etc,
# and add a keep-list so future updates preserve it.
#
# Usage:  sudo ./restore-after-steamos-update.sh [--with-nopasswd-sudo]
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "Run with sudo." >&2; exit 1; }

PREV=/etc/previous

# 1. udev rules: device access for the openlinkhub user (Corsair HID, Aura USB, i2c, uinput),
#    plus group write on the nct6798 motherboard pwm files.
install -Dm644 /opt/OpenLinkHub/99-openlinkhub.rules /etc/udev/rules.d/99-openlinkhub.rules
if [ -f "$PREV/udev/rules.d/99-motherboard-pwm.rules" ]; then
  install -Dm644 "$PREV/udev/rules.d/99-motherboard-pwm.rules" /etc/udev/rules.d/99-motherboard-pwm.rules
else
  cat > /etc/udev/rules.d/99-motherboard-pwm.rules <<'EOF'
SUBSYSTEM=="hwmon", KERNEL=="hwmon*", ATTR{name}=="nct6798", RUN+="/bin/sh -c 'chgrp -h openlinkhub /sys%p/pwm* 2>/dev/null || true; chmod g+rw /sys%p/pwm* 2>/dev/null || true'"
EOF
fi

# 2. Load the motherboard Super I/O fan driver at boot.
echo nct6775 > /etc/modules-load.d/nct6775.conf

# 2b. Keep the kernel corsair_psu driver off the HX1000i: it talks to the PSU at the same
#     time as OpenLinkHub and causes "response does not match the request" errors.
echo "blacklist corsair_psu" > /etc/modprobe.d/corsair-psu-blacklist.conf

# 3. Optional: restore passwordless sudo for deck (it existed before the update).
if [ "${1:-}" = "--with-nopasswd-sudo" ] && [ -f "$PREV/sudoers.d/zz-deck-nopasswd" ]; then
  install -m440 "$PREV/sudoers.d/zz-deck-nopasswd" /etc/sudoers.d/zz-deck-nopasswd
  visudo -c >/dev/null
fi

# 4. Tell future SteamOS updates to keep all of the above.
cat > /etc/atomic-update.conf.d/50-fans-and-leds.conf <<'EOF'
/etc/udev/rules.d/99-openlinkhub.rules
/etc/udev/rules.d/99-motherboard-pwm.rules
/etc/modules-load.d/nct6775.conf
/etc/modprobe.d/corsair-psu-blacklist.conf
/etc/sudoers.d/zz-deck-nopasswd
EOF

# 5. Apply now, without a reboot.
modprobe nct6775 || true
modprobe -r corsair_psu 2>/dev/null || true
udevadm control --reload-rules
udevadm trigger
udevadm settle
systemctl daemon-reload
systemctl restart OpenLinkHub
echo "Done. Check: tail -f /opt/OpenLinkHub/stdout.log"

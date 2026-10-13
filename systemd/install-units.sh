#!/usr/bin/env bash
# Install the factory systemd units on an agent host. Run as an admin with sudo:
#   sudo bash install-units.sh            # install units, leave timers disabled
#   sudo bash install-units.sh --enable   # install and start the timers
# Units live in /etc/systemd/system (root-owned): agents and the factory user
# cannot add or change schedules; only an admin arms them.
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
cd "$(dirname "$0")"
[[ $(id -u {{factory_user}}) == {{factory_uid}} ]] || { echo "units assume {{factory_user}} has uid {{factory_uid}}" >&2; exit 1; }
install -m 644 -o root -g root factory@.service factory-tick.timer factory-sweep.timer factory-clock.timer /etc/systemd/system/
systemctl daemon-reload
if [[ ${1:-} == --enable ]]; then
  systemctl enable --now factory-tick.timer factory-sweep.timer factory-clock.timer
fi
systemctl list-timers 'factory-*' --all --no-pager

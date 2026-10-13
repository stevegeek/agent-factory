#!/usr/bin/env bash
# Install the factory systemd units on an agent host. Run as an admin with sudo:
#   sudo bash install-units.sh                    # dispatcher: install units, timers disabled
#   sudo bash install-units.sh --enable           # dispatcher: install and start the timers
#   sudo bash install-units.sh --helper --enable  # helper host: only the weekly image rebuild
# Units live in /etc/systemd/system (root-owned): agents and the factory user
# cannot add or change schedules; only an admin arms them.
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
cd "$(dirname "$0")"
[[ $(id -u {{factory_user}}) == {{factory_uid}} ]] || { echo "units assume {{factory_user}} has uid {{factory_uid}}" >&2; exit 1; }
helper= enable=
for a in "$@"; do
  case $a in
    --helper) helper=1 ;;
    --enable) enable=1 ;;
    *) echo "unknown argument: $a" >&2; exit 2 ;;
  esac
done
timers=(factory-rebuild.timer)
units=(factory-rebuild.service factory-rebuild.timer)
if [[ -z $helper ]]; then
  timers+=(factory-tick.timer factory-sweep.timer factory-clock.timer)
  units+=(factory@.service factory-tick.timer factory-sweep.timer factory-clock.timer)
fi
install -m 644 -o root -g root "${units[@]}" /etc/systemd/system/
systemctl daemon-reload
if [[ -n $helper ]]; then
  # a helper never dispatches; make sure no dispatcher timer is left armed here
  systemctl disable --now factory-tick.timer factory-sweep.timer factory-clock.timer 2>/dev/null || true
fi
if [[ -n $enable ]]; then
  systemctl enable --now "${timers[@]}"
fi
systemctl list-timers 'factory-*' --all --no-pager

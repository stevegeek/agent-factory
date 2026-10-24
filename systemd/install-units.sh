#!/usr/bin/env bash
# Install the factory systemd units on an agent host. Run as an admin with sudo:
#   sudo bash install-units.sh                    # dispatcher: install units, timers disabled
#   sudo bash install-units.sh --enable           # dispatcher: install and start the timers
#   sudo bash install-units.sh --helper --enable  # helper host: only the weekly image rebuild
# --helper also deletes the dispatcher units (factory@.service and the tick, sweep and
# clock timers), not only disables them: updates.sh's reboot window runs
# `systemctl start factory-tick.timer` when it ends, and a unit file that is not
# there cannot be started, so no timer ticks on a helper (or a demoted master) again.
# Foreman's wake path is a user unit deploy.sh enables: a demoted master disables it
# by hand (README, "Moving the master").
# Re-running without --helper installs them back.
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
  # a helper never dispatches: disarm the dispatcher timers, then delete their units.
  # One timer at a time: systemctl refuses a disable naming a unit file that is not
  # there (an older install without the clock timer) and would then stop none of them.
  for t in factory-tick.timer factory-sweep.timer factory-clock.timer; do
    systemctl stop "$t" 2>/dev/null || true
    systemctl disable "$t" 2>/dev/null || true
  done
  rm -f /etc/systemd/system/timers.target.wants/factory-tick.timer \
    /etc/systemd/system/timers.target.wants/factory-sweep.timer \
    /etc/systemd/system/timers.target.wants/factory-clock.timer
  rm -f /etc/systemd/system/factory@.service /etc/systemd/system/factory-tick.timer \
    /etc/systemd/system/factory-sweep.timer /etc/systemd/system/factory-clock.timer
  systemctl daemon-reload
fi
if [[ -n $enable ]]; then
  systemctl enable --now "${timers[@]}"
fi
systemctl list-timers 'factory-*' --all --no-pager

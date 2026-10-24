#!/usr/bin/env bash
# Install the factory systemd units on an agent host. Run as an admin with sudo:
#   sudo bash install-units.sh                    # dispatcher: install units, timers disabled
#   sudo bash install-units.sh --enable           # dispatcher: install and start the timers
#   sudo bash install-units.sh --helper --enable  # helper host: only the weekly image rebuild
#   sudo bash install-units.sh --helper --watchdog --enable  # and the 5-minute check of the master
# --helper also deletes the dispatcher units (factory@.service and the tick, sweep and
# clock timers), not only disables them: updates.sh's reboot window runs
# `systemctl start factory-tick.timer` when it ends, and a unit file that is not
# there cannot be started, so no timer ticks on a helper (or a demoted master) again.
# Foreman's wake path is a user unit deploy.sh enables: a demoted master disables it
# by hand (README, "Moving the master").
# Re-running without --helper installs them back.
# --watchdog (helpers only) adds factory-watchdog.timer, factory-remote watchdog's
# outside check of the master, which only notifies. A run without --watchdog
# disarms and deletes any watchdog units, so opting out is re-running without it.
# Without --enable a run starts nothing and leaves an already-enabled timer
# enabled (the watchdog's as the rebuild's): only --enable arms a timer anew.
# Units live in /etc/systemd/system (root-owned): agents and the factory user
# cannot add or change schedules; only an admin arms them.
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
cd "$(dirname "$0")"
[[ $(id -u factory) == 1001 ]] || { echo "units assume factory has uid 1001" >&2; exit 1; }
helper= enable= watchdog=
for a in "$@"; do
  case $a in
    --helper) helper=1 ;;
    --enable) enable=1 ;;
    --watchdog) watchdog=1 ;;
    *) echo "unknown argument: $a" >&2; exit 2 ;;
  esac
done
[[ -z $watchdog || -n $helper ]] || { echo "--watchdog is for a helper: use it with --helper" >&2; exit 2; }
timers=(factory-rebuild.timer)
units=(factory-rebuild.service factory-rebuild.timer)
if [[ -z $helper ]]; then
  timers+=(factory-tick.timer factory-sweep.timer factory-clock.timer)
  units+=(factory@.service factory-tick.timer factory-sweep.timer factory-clock.timer)
fi
if [[ -n $watchdog ]]; then
  timers+=(factory-watchdog.timer)
  units+=(factory-watchdog.service factory-watchdog.timer)
fi
# every unit file is here and a plain file (not a link to elsewhere) before anything changes:
# a bundle from before a unit existed stops here rather than half-installed
for u in "${units[@]}"; do
  [[ -f $u && ! -L $u ]] || { echo "$u: missing or not a plain file here; deploy this host first" >&2; exit 1; }
done
install -m 644 -o root -g root "${units[@]}" /etc/systemd/system/
systemctl daemon-reload
if [[ -z $watchdog ]]; then
  # not opted in (or opted out): disarm the watchdog, then delete its units, and say so,
  # so a routine re-run without --watchdog does not end the watch unnoticed
  if [[ -e /etc/systemd/system/factory-watchdog.timer ]]; then
    echo "install-units.sh: removed the watchdog (factory-watchdog.timer); re-run with --watchdog to keep it" >&2
  fi
  for u in factory-watchdog.timer factory-watchdog.service; do
    systemctl stop "$u" 2>/dev/null || true
  done
  systemctl disable factory-watchdog.timer 2>/dev/null || true
  rm -f /etc/systemd/system/timers.target.wants/factory-watchdog.timer \
    /etc/systemd/system/factory-watchdog.timer /etc/systemd/system/factory-watchdog.service
  systemctl daemon-reload
fi
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

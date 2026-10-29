#!/usr/bin/env bash
# Install the factory systemd units on an agent host, as root. host/setup.sh puts a root-owned
# copy of this script and the unit templates in /usr/local/lib/<name>/units and links it as
# /usr/local/sbin/<name>-install-units (<name> is setup.sh's FACTORY_NAME, default agent-factory):
#   sudo agent-factory-install-units                    # dispatcher: install units, timers disabled
#   sudo agent-factory-install-units --enable           # dispatcher: install and start the timers
#   sudo agent-factory-install-units --helper --enable  # helper host: only the weekly image rebuild
#   sudo agent-factory-install-units --helper --watchdog --enable  # and the 5-minute check of the master
# Optional: FACTORY_USER (default factory), as given to setup.sh.
# The units become root's: they come only from that copy, never from ~factory/factory/systemd,
# which the factory user (and so an agent) can edit. This script refuses to run from a
# directory, or with a unit file, that anyone but root can change, and fills the units'
# placeholders itself from the system's record of the factory user. Only the rebuild time
# ([upkeep] rebuild_at) is read from the deployed factory-rebuild.timer, and only as a plain
# calendar expression. After a deploy that changes a unit or this script, re-run setup.sh
# from a checkout of that commit, then this.
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
die() { echo "install-units.sh: $*" >&2; exit 1; }
self=$(readlink -f "$0")
cd "${self%/*}"
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
# root_only PATH: not a link, owned by root, and writable by no one else
root_only() { [[ ! -L $1 ]] && [[ $(stat -c %u "$1") == 0 ]] && (( (8#$(stat -c %a "$1") & 8#022) == 0 )); }
root_only . && root_only "$self" || die "${self%/*} is not root's alone: run the copy host/setup.sh installs"
# every unit file is here and root's before anything changes: a copy from before a unit
# existed stops here rather than half-installed
for u in "${units[@]}"; do
  [[ -f $u ]] || die "$u: missing here; re-run host/setup.sh from a checkout that has it"
  root_only "$u" || die "$u: not root's alone here: run the copy host/setup.sh installs"
done
user=${FACTORY_USER:-factory}
[[ $user =~ ^[a-z_][a-z0-9_-]*$ ]] || die "$user is not a plain user name"
uid=$(id -u "$user" 2>/dev/null) || die "no user $user"
[[ $uid != 0 ]] || die "refusing units that would run as root"
home=$(getent passwd "$user" | cut -d: -f6)
[[ $home == /?* && $home != *[!A-Za-z0-9/._-]* ]] || die "$user's home ($home) is not a plain absolute path"
root=${home%/}/factory
timer=$root/systemd/factory-rebuild.timer
at=
if [[ -f $timer && ! -L $timer ]]; then
  at=$(sed -n '/^OnCalendar=/{s///p;q}' "$timer")
fi
calendar='^[A-Za-z0-9*:.,/~+ -]+$'
[[ $at =~ $calendar ]] || die "$timer: no plain OnCalendar= line; deploy this host first"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
for u in "${units[@]}"; do
  s=$(<"$u")
  for kv in "factory_user=$user" "factory_uid=$uid" "factory_home=$home" "factory_root=$root" "rebuild_at=$at"; do
    s=${s//"{{${kv%%=*}}}"/"${kv#*=}"}
  done
  [[ $s != *'{{'* ]] || die "$u: has a placeholder this script does not fill"
  printf '%s\n' "$s" >"$out/$u"
done
install -m 644 -o root -g root "${units[@]/#/$out/}" /etc/systemd/system/
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

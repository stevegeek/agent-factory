#!/usr/bin/env bash
# OS updates for a factory host. Run as root on the host:
#   sudo bash updates.sh                    # apply (safe to re-run)
#   sudo REBOOT_AT="Sun *-*-* 04:30:00 UTC" bash updates.sh
#   sudo bash updates.sh --undo
# Optional: FACTORY_NAME (names the files written here), FACTORY_USER (default factory),
# REBOOT_AT (systemd calendar, default "Sun *-*-* 04:00:00", host time zone unless named).
#
# Does:
#   1. unattended-upgrades also installs <release>-updates, not only -security
#      (still daily, through Ubuntu's apt-daily timers)
#   2. a weekly reboot window: only if an update asked for one (/run/reboot-required),
#      stop the factory tick timer and drain helper jobs so no new work starts, wait up
#      to 90 minutes for agents, an image rebuild or apt to finish, then reboot. The
#      tick timer is enabled, so it comes back at boot. With no reboot pending it does
#      nothing.
set -euo pipefail

NAME=${FACTORY_NAME:-agent-factory}
FACTORY_USER=${FACTORY_USER:-factory}
REBOOT_AT=${REBOOT_AT:-Sun *-*-* 04:00:00}
APT_CONF=/etc/apt/apt.conf.d/52$NAME-updates
SCRIPT=/usr/local/sbin/$NAME-weekly-reboot
UNIT=$NAME-weekly-reboot

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "run with sudo"

if [[ ${1:-} == --undo ]]; then
  systemctl disable --now "$UNIT.timer" 2>/dev/null || true
  systemctl stop "$UNIT.service" 2>/dev/null || true
  rm -f "/etc/systemd/system/$UNIT.timer" "/etc/systemd/system/$UNIT.service" "$SCRIPT" "$APT_CONF" \
    /run/agent-factory-draining
  systemctl daemon-reload
  systemctl start factory-tick.timer 2>/dev/null || true
  echo "removed the weekly reboot window and the -updates origin"
  exit 0
fi
[[ -z ${1:-} ]] || die "usage: updates.sh [--undo]"
id "$FACTORY_USER" >/dev/null 2>&1 || die "user $FACTORY_USER not found"
systemd-analyze calendar "$REBOOT_AT" >/dev/null || die "REBOOT_AT is not a systemd calendar expression: $REBOOT_AT"
dpkg -s unattended-upgrades >/dev/null 2>&1 || die "unattended-upgrades is not installed"

cat > "$APT_CONF" <<'EOF'
// agent factory: take regular updates too, not only security ones
Unattended-Upgrade::Allowed-Origins:: "${distro_id}:${distro_codename}-updates";
// reboots happen in the weekly window instead
Unattended-Upgrade::Automatic-Reboot "false";
EOF
chmod 644 "$APT_CONF"

cat > "$SCRIPT" <<EOF
#!/bin/bash
# Weekly reboot window (written by updates.sh). Reboots only when an update asked for it.
set -u
if [[ ! -f /run/reboot-required ]]; then echo "no reboot pending"; exit 0; fi
echo "reboot pending for: \$(tr '\n' ' ' < /run/reboot-required.pkgs 2>/dev/null)"
# no new work: the dispatcher's tick, and helper jobs (factory-remote refuses while this exists;
# /run is cleared by the reboot)
systemctl stop factory-tick.timer 2>/dev/null || true
touch /run/agent-factory-draining
busy() {
  local s
  # oneshot units report "activating" while they run, so ask for the state itself
  for u in factory@tick.service factory-rebuild.service apt-daily-upgrade.service; do
    s=\$(systemctl show -P ActiveState "\$u" 2>/dev/null)
    [[ -n \$s && \$s != inactive && \$s != failed ]] && { echo "\$u is \$s"; return 0; }
  done
  pgrep -u $FACTORY_USER -x conmon >/dev/null && { echo "containers running"; return 0; }
  pgrep -u $FACTORY_USER -f 'factory-remote run' >/dev/null && { echo "helper jobs running"; return 0; }
  return 1
}
for _ in \$(seq 18); do
  busy || break
  echo "waiting 5 minutes"
  sleep 300
done
echo "rebooting"
systemctl reboot
# stay up until shutdown stops this unit; the stop hooks clean up only if no reboot came
sleep 900
EOF
chmod 755 "$SCRIPT"

cat > "/etc/systemd/system/$UNIT.service" <<EOF
[Unit]
Description=Agent factory: weekly reboot window (only when an update needs one)

[Service]
Type=oneshot
ExecStart=$SCRIPT
TimeoutStartSec=2h
# if the reboot did not happen, let work resume
ExecStopPost=-/bin/rm -f /run/agent-factory-draining
ExecStopPost=-/bin/systemctl --no-block start factory-tick.timer
EOF

cat > "/etc/systemd/system/$UNIT.timer" <<EOF
[Unit]
Description=Agent factory: weekly reboot window

[Timer]
OnCalendar=$REBOOT_AT

[Install]
WantedBy=timers.target
EOF
chmod 644 "/etc/systemd/system/$UNIT.service" "/etc/systemd/system/$UNIT.timer"
systemctl daemon-reload
systemctl enable --now "$UNIT.timer"

echo "-- unattended-upgrades origins"
apt-config dump | grep -A4 '^Unattended-Upgrade::Allowed-Origins' | grep -v '^Unattended-Upgrade::Allowed-Origins ""' || true
echo "-- next reboot window"
systemctl list-timers "$UNIT.timer" --no-pager | head -2
echo "-- reboot pending now: $([[ -f /run/reboot-required ]] && echo yes || echo no)"

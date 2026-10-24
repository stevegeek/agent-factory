#!/usr/bin/env bash
# Agent host base setup (Ubuntu 26.04). Run as root on the host:
#   sudo ADMIN_USER=you bash setup.sh          # apply (safe to re-run)
#   sudo ADMIN_USER=you bash setup.sh --undo   # remove config written here
# Optional: FACTORY_USER (default factory), FACTORY_NAME (default agent-factory;
# names the config files written here), BUDGET_* (below).
#
# Does:
#   1. zram swap (zstd, half of RAM) in front of the existing /swap.img
#   2. swappiness tuned for zram; systemd-oomd enabled, sshd and the admin
#      user's sessions shielded from it
#   3. 'factory' user: no sudo, locked password, rootless Podman, lingering
#      user manager. Its SSH key lives in a root-owned file under /etc/ssh
#      (the user cannot add keys) and agent/TCP forwarding is off for it.
#      Two more root-owned files hold keys added by other scripts, and this
#      script never writes them, so a re-run keeps them: <user>.dispatchers
#      (allow-dispatcher.sh) and <user>.pullers (allow-puller.sh).
#   4. one memory/CPU/task budget for everything the factory user runs,
#      so sshd and the host always keep headroom
#
# --undo leaves the factory user, its home and the packages in place
# (it prints the commands to remove them).
set -euo pipefail

FACTORY_USER=${FACTORY_USER:-factory}
ADMIN_USER=${ADMIN_USER:?set ADMIN_USER to the sudo-capable admin account}
NAME=${FACTORY_NAME:-agent-factory}
ZRAM_CONF=/etc/systemd/zram-generator.conf
SYSCTL_CONF=/etc/sysctl.d/90-$NAME.conf
OOMD_DROPIN=/etc/systemd/oomd.conf.d/90-$NAME.conf
SSH_SERVICE_DROPIN=/etc/systemd/system/ssh.service.d/90-$NAME.conf
SSHD_DROPIN=/etc/ssh/sshd_config.d/70-$NAME.conf
FACTORY_KEYS_DIR=/etc/ssh/authorized_keys
SLICE_DROPIN_NAME=50-$NAME.conf

# Budget for the whole factory user (all agent containers together).
# Defaults suit a ~3.3 GiB host (zram is not charged to the slice), so the caps
# stay well below RAM to leave sshd and the OS room. For a 2 GiB host:
#   BUDGET_MEMORY_HIGH=1000M BUDGET_MEMORY_MAX=1200M BUDGET_SWAP_MAX=768M
BUDGET_MEMORY_HIGH=${BUDGET_MEMORY_HIGH:-1500M}    # start reclaiming/throttling here
BUDGET_MEMORY_MAX=${BUDGET_MEMORY_MAX:-1800M}      # hard ceiling; OOM-kill inside the slice
BUDGET_SWAP_MAX=${BUDGET_SWAP_MAX:-1G}             # roughly zram-sized, keeps disk swap out of it
BUDGET_TASKS_MAX=2048
BUDGET_CPU_WEIGHT=50        # host processes win under contention

log() { printf '\n== %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run with sudo"
. /etc/os-release
[[ ${ID:-} == ubuntu ]] || die "expected Ubuntu, found ${ID:-unknown}"
id "$ADMIN_USER" >/dev/null 2>&1 || die "admin user $ADMIN_USER not found"
admin_uid=$(id -u "$ADMIN_USER")

# Writes $2 to file $1 only if the content differs. Returns 0 when changed.
write_if_changed() {
  local path=$1 content=$2
  if [[ -f $path ]] && [[ $(cat "$path") == "$content" ]]; then
    return 1
  fi
  mkdir -p "$(dirname "$path")"
  printf '%s\n' "$content" > "$path"
  return 0
}

sshd_reload_checked() {
  if ! sshd -t; then
    echo "sshd config invalid; removing $SSHD_DROPIN" >&2
    rm -f "$SSHD_DROPIN"
    die "sshd config test failed (nothing reloaded)"
  fi
  systemctl try-reload-or-restart ssh
}

undo() {
  log "Removing factory config"
  if id "$FACTORY_USER" >/dev/null 2>&1; then
    uid=$(id -u "$FACTORY_USER")
    rm -f "/etc/systemd/system/user-$uid.slice.d/$SLICE_DROPIN_NAME"
    rmdir "/etc/systemd/system/user-$uid.slice.d" 2>/dev/null || true
    loginctl disable-linger "$FACTORY_USER" || true
  fi
  rm -f "/etc/systemd/system/user-$admin_uid.slice.d/$SLICE_DROPIN_NAME"
  rmdir "/etc/systemd/system/user-$admin_uid.slice.d" 2>/dev/null || true
  rm -f "$SSHD_DROPIN" "$FACTORY_KEYS_DIR/$FACTORY_USER" "$FACTORY_KEYS_DIR/$FACTORY_USER.dispatchers" \
    "$FACTORY_KEYS_DIR/$FACTORY_USER.pullers"
  rmdir "$FACTORY_KEYS_DIR" 2>/dev/null || true
  { sshd -t && systemctl try-reload-or-restart ssh; } || true
  rm -f "$SYSCTL_CONF" "$OOMD_DROPIN" "$SSH_SERVICE_DROPIN"
  if [[ -f $ZRAM_CONF ]] && grep -q "$NAME" "$ZRAM_CONF"; then
    swapoff /dev/zram0 2>/dev/null || true
    rm -f "$ZRAM_CONF"
    latest_bak=$(ls -1t "$ZRAM_CONF".bak.* 2>/dev/null | head -1 || true)
    if [[ -n $latest_bak ]]; then
      mv "$latest_bak" "$ZRAM_CONF"
      echo "restored $ZRAM_CONF from $latest_bak"
      systemctl daemon-reload
      systemctl restart systemd-zram-setup@zram0.service || true
    fi
  fi
  systemctl daemon-reload
  sysctl --system >/dev/null
  systemctl try-restart systemd-oomd.service || true
  cat <<EOF

Done. Left in place on purpose:
  - user '$FACTORY_USER' and /home/$FACTORY_USER (may hold agent work)
      remove with: sudo userdel -r $FACTORY_USER
  - systemd-oomd stays enabled: sudo systemctl disable --now systemd-oomd
  - packages: sudo apt purge systemd-zram-generator systemd-oomd podman passt uidmap
EOF
  exit 0
}

[[ ${1:-} == --undo ]] && undo
[[ $# -eq 0 ]] || die "unknown argument: $1"

log "Installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq systemd-zram-generator systemd-oomd podman passt uidmap

log "zram swap (zstd, ram/2, priority 100; /swap.img stays as fallback)"
if [[ -f $ZRAM_CONF ]] && ! grep -q "$NAME" "$ZRAM_CONF"; then
  cp -a "$ZRAM_CONF" "$ZRAM_CONF.bak.$(date +%s)"
fi
zram_content="# $NAME: compressed RAM swap, used before the disk swap file
[zram0]
zram-size = ram / 2
compression-algorithm = zstd
swap-priority = 100"
if write_if_changed "$ZRAM_CONF" "$zram_content"; then
  systemctl daemon-reload
  systemctl restart systemd-zram-setup@zram0.service
fi
systemctl start dev-zram0.swap

log "VM tuning for zram"
write_if_changed "$SYSCTL_CONF" "# $NAME: swap to zram readily (cheap), small swap-in reads
vm.swappiness = 150
vm.page-cluster = 0" || true
sysctl --system >/dev/null

log "systemd-oomd, with sshd and $ADMIN_USER shielded"
write_if_changed "$OOMD_DROPIN" "# $NAME
[OOM]
SwapUsedLimit=90%
DefaultMemoryPressureDurationSec=20s" || true
write_if_changed "$SSH_SERVICE_DROPIN" "# $NAME: never pick sshd as the OOM victim
[Service]
ManagedOOMPreference=omit
MemoryMin=64M" || true
write_if_changed "/etc/systemd/system/user-$admin_uid.slice.d/$SLICE_DROPIN_NAME" \
"# $NAME: prefer killing something other than the admin's sessions
[Slice]
ManagedOOMPreference=avoid" || true
systemctl daemon-reload
systemctl enable systemd-oomd.service
systemctl restart systemd-oomd.service

log "User '$FACTORY_USER'"
if ! id "$FACTORY_USER" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash --user-group "$FACTORY_USER"
fi
passwd --lock "$FACTORY_USER" >/dev/null    # SSH key only, no password
for g in sudo adm admin wheel lxd docker; do
  if id -nG "$FACTORY_USER" | tr ' ' '\n' | grep -qx "$g"; then
    gpasswd -d "$FACTORY_USER" "$g" >/dev/null
  fi
done
uid=$(id -u "$FACTORY_USER")
grep -q "^$FACTORY_USER:" /etc/subuid || die "no /etc/subuid range for $FACTORY_USER (useradd should add one)"
grep -q "^$FACTORY_USER:" /etc/subgid || die "no /etc/subgid range for $FACTORY_USER (useradd should add one)"

log "SSH for '$FACTORY_USER': root-owned key file, no forwarding"
# Root-owned key under /etc/ssh: nothing is written into the factory home,
# and agents cannot add their own keys.
src_keys=/home/$ADMIN_USER/.ssh/authorized_keys
[[ -s $src_keys && ! -L $src_keys ]] || die "$src_keys missing or a symlink"
install -d -m 755 -o root -g root "$FACTORY_KEYS_DIR"
install -m 644 -o root -g root "$src_keys" "$FACTORY_KEYS_DIR/$FACTORY_USER"
sshd_content="# $NAME
Match User $FACTORY_USER
    AuthorizedKeysFile $FACTORY_KEYS_DIR/%u $FACTORY_KEYS_DIR/%u.dispatchers $FACTORY_KEYS_DIR/%u.pullers
    AllowAgentForwarding no
    AllowTcpForwarding no
    AllowStreamLocalForwarding no
    X11Forwarding no
    PermitTunnel no"
if write_if_changed "$SSHD_DROPIN" "$sshd_content"; then
  # Check the effective config on disk before reloading: the Match block
  # must apply to factory and must not leak onto the admin account.
  admin_cfg=$(sshd -T -C "user=$ADMIN_USER,host=localhost,addr=127.0.0.1" || true)
  factory_cfg=$(sshd -T -C "user=$FACTORY_USER,host=localhost,addr=127.0.0.1" || true)
  if ! grep -qix 'allowagentforwarding yes' <<<"$admin_cfg" \
     || ! grep -qix 'allowagentforwarding no' <<<"$factory_cfg"; then
    rm -f "$SSHD_DROPIN"
    die "sshd Match block did not apply as expected; removed $SSHD_DROPIN (nothing reloaded)"
  fi
  sshd_reload_checked
fi

# User manager runs without a login session (needed for rootless containers
# started by timers).
loginctl enable-linger "$FACTORY_USER"

log "Resource budget for user-$uid.slice (all factory processes)"
write_if_changed "/etc/systemd/system/user-$uid.slice.d/$SLICE_DROPIN_NAME" \
"# $NAME: one shared budget for every agent container
[Slice]
MemoryHigh=$BUDGET_MEMORY_HIGH
MemoryMax=$BUDGET_MEMORY_MAX
MemorySwapMax=$BUDGET_SWAP_MAX
TasksMax=$BUDGET_TASKS_MAX
CPUWeight=$BUDGET_CPU_WEIGHT
ManagedOOMMemoryPressure=kill
ManagedOOMMemoryPressureLimit=80%" || true
systemctl daemon-reload
systemctl start "user@$uid.service"

log "Verification"
set +e
echo "-- swap";   swapon --show
echo "-- zram";   zramctl
echo "-- sysctl"; sysctl vm.swappiness vm.page-cluster
echo "-- oomd";   systemctl is-active systemd-oomd
echo "-- user";   id "$FACTORY_USER"; loginctl show-user "$FACTORY_USER" -p Linger
echo "-- budget"
systemctl show "user-$uid.slice" -p MemoryHigh -p MemoryMax -p MemorySwapMax -p TasksMax -p CPUWeight
echo "-- sshd ($FACTORY_USER)"
factory_cfg=$(sshd -T -C "user=$FACTORY_USER,host=localhost,addr=127.0.0.1")
grep -Ei '^(authorizedkeysfile|allowagentforwarding|allowtcpforwarding)' <<<"$factory_cfg"
echo "-- podman (as $FACTORY_USER)"
(cd / && runuser -u "$FACTORY_USER" -- env XDG_RUNTIME_DIR="/run/user/$uid" \
  podman info --format 'version={{.Version.Version}} rootless={{.Host.Security.Rootless}} cgroups={{.Host.CgroupsVersion}} controllers={{.Host.CgroupControllers}}')
set -e

cat <<EOF

All set. Next: ssh $FACTORY_USER@$(hostname -I | awk '{print $1}')
'controllers' above must include 'memory' for per-container limits.
EOF

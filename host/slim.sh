#!/usr/bin/env bash
# Strip a factory host (Ubuntu 26.04 server) down to what the factory uses: frees disk
# and resident memory. Things removed here can be reinstalled with apt when needed.
#   bash slim.sh                    # dry run (no root needed): plan + apt simulation
#   sudo bash slim.sh --apply       # do it, detached from the SSH session (log in
#                                   # /var/log/agent-factory-slim.log); then reboot
#   sudo bash slim.sh --undo        # reinstall what --apply removed, restore apt marks
# Optional: KEEP_FIRMWARE (default "realtek intel-graphics intel-misc": the r8169 NIC
# and Intel graphics firmware), KEEP_PACKAGES (names to leave installed).
#
# Removes, when installed:
#   snapd (and its snaps), cloud-init, kdump + the crashkernel RAM reservation,
#   storage stacks not in use (multipath, iSCSI, LVM, mdadm, bcache, btrfs, xfs),
#   desktop/laptop daemons (ModemManager, udisks2, upower, fwupd, packagekit, polkitd,
#   networkd-dispatcher, gpu-manager, plymouth), reporting (apport, sysstat, motd news,
#   Ubuntu Pro client, landscape), rsyslog (journald keeps the logs), tracing tools
#   (bpftrace, bcc, perf, crash, LLVM), kernel headers and tools, config left behind by
#   kernels removed earlier, firmware for hardware these boxes do not have.
# Keeps: every installed kernel image (the newest boots, an older one stays as a GRUB
# fallback; apt's autoremove retires them as updates arrive), linux-image-generic so
# updates still arrive, ssh, podman and its stack, systemd-networkd/resolved, netplan,
# chrony, unattended-upgrades, sudo, git, python3, thermald, zram, dracut.
# Also: marks the rest manual first, so removing a metapackage (ubuntu-server-minimal
# etc.) cannot cascade; caps the journal at 64M; pins snapd out; turns off Recommends
# for later installs; apt clean; backs up and checks the initrd before advising a reboot.
set -euo pipefail

STATE=/var/lib/agent-factory-slim
LOG=/var/log/agent-factory-slim.log
KEEP_FIRMWARE=${KEEP_FIRMWARE:-realtek intel-graphics intel-misc}
KEEP_PACKAGES=${KEEP_PACKAGES:-}
JOURNAL_CONF=/etc/systemd/journald.conf.d/90-agent-factory-slim.conf
APT_CONF=/etc/apt/apt.conf.d/90-agent-factory-slim
SNAP_PIN=/etc/apt/preferences.d/90-agent-factory-no-snapd

# Never removed, whatever apt resolves.
PROTECTED="openssh-server openssh-client openssh-sftp-server systemd systemd-sysv systemd-resolved
  systemd-oomd libpam-systemd dbus dbus-user-session udev netplan.io netplan-generator
  podman crun runc conmon passt slirp4netns uidmap fuse-overlayfs containers-storage golang-github-containers-common
  linux-image-generic intel-microcode amd64-microcode linux-firmware-minimal sudo sudo-rs apt dpkg
  unattended-upgrades chrony git python3 thermald systemd-zram-generator zram-tools dracut dracut-core
  grub-efi-amd64 grub-efi-amd64-bin grub-efi-amd64-signed grub-pc grub-pc-bin shim-signed grub2-common grub-common
  ca-certificates curl $(for f in $KEEP_FIRMWARE; do printf 'linux-firmware-%s ' "$f"; done) $KEEP_PACKAGES"

CANDIDATES="snapd lxd-installer lxd-agent-loader cloud-init cloud-guest-utils cloud-initramfs-copymods
  cloud-initramfs-dyn-netconf kdump-tools linux-crashdump makedumpfile kexec-tools crash
  multipath-tools libmultipath0 libmpathpersist0 libmpathcmd0 open-iscsi libopeniscsiusr bcache-tools
  btrfs-progs xfsprogs modemmanager udisks2 upower fwupd fwupd-signed packagekit packagekit-tools polkitd
  networkd-dispatcher ubuntu-drivers-common plymouth plymouth-theme-ubuntu-text apport apport-core-dump-handler
  apport-symptoms python3-apport whoopsie popularity-contest sysstat motd-news-config update-notifier-common
  ubuntu-pro-client ubuntu-pro-client-l10n ubuntu-advantage-tools landscape-common open-vm-tools
  rsyslog bpftrace bpfcc-tools python3-bpfcc libbpfcc linux-perf bpftool linux-tools-common
  libllvm21 libclang-cpp21 libclang1-21 python3-boto3 python3-botocore python3-s3transfer
  linux-generic linux-headers-generic firmware-sof-signed linux-firmware lvm2 mdadm cryptsetup cryptsetup-initramfs"

KERNEL_PKGS='^linux-(image|image-unsigned|modules|modules-extra|main-modules-[a-z]+)-[0-9]'

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
installed() { dpkg-query -W -f='${db:Status-Abbrev}' "$1" 2>/dev/null | grep -q '^ii'; }
in_list() { local x=$1; shift; [[ " $* " == *" $x "* ]]; }

mode=dry
case ${1:-} in
  --apply) mode=apply ;;
  --undo) mode=undo ;;
  "") ;;
  *) die "usage: slim.sh [--apply | --undo]" ;;
esac
[[ $mode == dry || $EUID -eq 0 ]] || die "run with sudo"
. /etc/os-release
[[ ${ID:-} == ubuntu ]] || die "expected Ubuntu, found ${ID:-unknown}"
script=""
if [[ -n ${BASH_SOURCE[0]:-} && -f ${BASH_SOURCE[0]} ]]; then script=$(readlink -f "${BASH_SOURCE[0]}"); fi

# A dropped SSH session must not kill apt halfway: --apply/--undo re-run detached.
if [[ $mode != dry && -z ${SLIM_DETACHED:-} ]]; then
  [[ -f $script ]] || die "--apply/--undo need the script as a file (not piped)"
  : > "$LOG"
  SLIM_DETACHED=1 KEEP_FIRMWARE="$KEEP_FIRMWARE" KEEP_PACKAGES="$KEEP_PACKAGES" \
    setsid nohup bash "$script" "$1" >> "$LOG" 2>&1 < /dev/null &
  pid=$!
  echo "running detached as pid $pid; log: $LOG (safe to disconnect; re-attach with: tail -f $LOG)"
  tail --pid="$pid" -f "$LOG"
  wait "$pid" 2>/dev/null || true
  exit 0
fi

running=$(uname -r)
running_ver=$(grep -oE '^[0-9]+\.[0-9]+\.[0-9]+-[0-9]+' <<<"$running" || true)
[[ -n $running_ver ]] || die "cannot parse running kernel version from $running"

if [[ $mode == undo ]]; then
  [[ -s $STATE/removed.txt ]] || die "nothing recorded in $STATE/removed.txt"
  rm -f "$SNAP_PIN" "$APT_CONF" "$JOURNAL_CONF"
  systemctl restart systemd-journald
  apt-get update -q
  # versioned kernel packages come back through linux-generic; skip anything the archive no longer has
  want=()
  while read -r p; do
    [[ -n $p ]] || continue
    [[ $p =~ -[0-9]+\.[0-9]+\.[0-9]+-[0-9]+ ]] && continue
    cand=$(apt-cache policy "$p" 2>/dev/null | awk '/Candidate:/{print $2}')
    [[ -n $cand && $cand != "(none)" ]] && want+=("$p") || echo "skipping $p: not in the archive"
  done < "$STATE/removed.txt"
  DEBIAN_FRONTEND=noninteractive apt-get install -y "${want[@]}"
  [[ -s $STATE/auto-before.txt ]] && xargs -a "$STATE/auto-before.txt" apt-mark auto >/dev/null 2>&1 || true
  update-grub
  echo "reinstalled ${#want[@]} packages (snaps are not restored); reboot to restore the kdump reservation"
  exit 0
fi

remove=()
skip_reason() {
  case $1 in
    lvm2) command -v pvs >/dev/null && [[ -n $(pvs --noheadings 2>/dev/null || true) ]] && echo "LVM physical volumes in use" ;;
    mdadm) grep -q '^md' /proc/mdstat 2>/dev/null && echo "md arrays present" ;;
    cryptsetup|cryptsetup-initramfs) lsblk -rno TYPE 2>/dev/null | grep -qx crypt && echo "encrypted devices present" ;;
    btrfs-progs) [[ $(findmnt -no FSTYPE /) == btrfs ]] && echo "root is btrfs" ;;
    xfsprogs) [[ $(findmnt -no FSTYPE /) == xfs ]] && echo "root is xfs" ;;
  esac
  return 0
}

for p in $CANDIDATES; do
  installed "$p" || continue
  in_list "$p" $KEEP_PACKAGES && continue
  why=$(skip_reason "$p")
  if [[ -n $why ]]; then echo "keeping $p: $why"; continue; fi
  remove+=("$p")
done

# Kernel headers and tools, any version (nothing here builds modules).
while read -r pkg; do
  in_list "$pkg" $KEEP_PACKAGES || remove+=("$pkg")
done < <(dpkg-query -W -f='${db:Status-Abbrev} ${Package}\n' 'linux-*' 2>/dev/null \
         | awk '$1=="ii"{print $2}' | grep -E '^linux-(headers|tools)-[0-9]' || true)

# Firmware: keep the sets named in KEEP_FIRMWARE, drop the rest (linux-firmware-minimal replaces the meta).
while read -r pkg; do
  in_list "${pkg#linux-firmware-}" $KEEP_FIRMWARE || remove+=("$pkg")
done < <(dpkg-query -W -f='${db:Status-Abbrev} ${Package}\n' 'linux-firmware-*' 2>/dev/null \
         | awk '$1=="ii"{print $2}' | grep -vx 'linux-firmware-minimal' || true)

# Config left behind by kernels removed earlier; never anything of the running kernel, never grub.
mapfile -t leftovers < <(dpkg-query -W -f='${db:Status-Abbrev} ${Package}\n' 2>/dev/null \
  | awk '$1=="rc"{print $2}' | grep -E "$KERNEL_PKGS|^linux-(headers|tools)-[0-9]" | grep -v -- "$running_ver" || true)

if [[ ${#remove[@]} -gt 0 ]]; then
  mapfile -t remove < <(printf '%s\n' "${remove[@]}" | sort -u)
fi
[[ ${#remove[@]} -gt 0 || ${#leftovers[@]} -gt 0 ]] || { echo "nothing to remove"; exit 0; }

echo "== removing (${#remove[@]}): ${remove[*]:-}"
echo "== installing: linux-firmware-minimal (provides linux-firmware)"
echo "== leftover kernel config purged: ${leftovers[*]:-none}"

# The same single transaction is what --apply runs.
apt_args=(purge "${remove[@]}" linux-firmware-minimal+)
sim=$(apt-get -s "${apt_args[@]}" 2>&1) || { echo "$sim" | tail -20; die "apt simulation failed"; }
purged=$(awk '/^(Purg|Remv) /{print $2}' <<<"$sim" | sort -u)
bad=""
for p in $purged; do
  in_list "$p" $PROTECTED && bad+=" $p"
  [[ $p =~ $KERNEL_PKGS ]] && bad+=" $p"
done
[[ -z $bad ]] || die "apt would also remove protected packages:$bad (nothing changed)"
extra=$(comm -13 <(printf '%s\n' "${remove[@]}" | sort -u) <(echo "$purged") | tr '\n' ' ')
echo "== apt also removes (dependents of the above): ${extra:-none}"
echo "== apt removes $(wc -w <<<"$purged") packages in total"
# shellcheck disable=SC2086
kb=$(dpkg-query -W -f='${Installed-Size}\n' $purged 2>/dev/null | awk '{s+=$1} END{print s+0}')
echo "== packages free about $(( kb / 1024 )) MB$(installed snapd && echo ", plus snaps: $(du -sh /var/lib/snapd 2>/dev/null | cut -f1)")"
echo "== kdump reservation now: $(( $(cat /sys/kernel/kexec_crash_size 2>/dev/null || echo 0) / 1048576 )) MB (freed after reboot)"
echo "== apt cache now: $(du -sh /var/cache/apt 2>/dev/null | cut -f1); journal: $(journalctl --disk-usage 2>/dev/null | grep -oE '[0-9.]+[KMG]' | head -1 || true)"

if [[ $mode == dry ]]; then
  echo
  echo "dry run only; apply with: sudo bash ${script:-slim.sh} --apply"
  exit 0
fi

echo "== $(date -Is) applying"
install -d -m 755 "$STATE"
ts=$(date +%Y%m%d-%H%M%S)
apt-mark showauto > "$STATE/auto-before.txt"
printf '%s\n' $purged > "$STATE/removed.txt"
cp -a /etc/netplan "$STATE/netplan-$ts"
cp -a "/boot/initrd.img-$running" "$STATE/initrd.img-$running.$ts"

# Everything installed becomes manual, so removing a metapackage cannot cascade ...
apt-mark showauto | xargs -r apt-mark manual >/dev/null
apt-mark manual linux-image-generic >/dev/null

export DEBIAN_FRONTEND=noninteractive
apt-get -y "${apt_args[@]}"
[[ ${#leftovers[@]} -eq 0 ]] || dpkg --purge "${leftovers[@]}"

# ... except kernel images and modules, so apt's usual autoremove still retires old ones.
dpkg-query -W -f='${db:Status-Abbrev} ${Package}\n' | awk '$1=="ii"{print $2}' | grep -E "$KERNEL_PKGS" \
  | xargs -r apt-mark auto >/dev/null

# cloud-init's purge must not take the network config with it.
if ! diff -rq "$STATE/netplan-$ts" /etc/netplan >/dev/null; then
  cp -a "$STATE/netplan-$ts/." /etc/netplan/
  echo "restored /etc/netplan from $STATE/netplan-$ts"
fi
netplan generate

mkdir -p "$(dirname "$JOURNAL_CONF")"
printf '[Journal]\nSystemMaxUse=64M\n' > "$JOURNAL_CONF"
systemctl restart systemd-journald
journalctl --vacuum-size=64M >/dev/null

printf 'APT::Install-Recommends "false";\nAPT::Install-Suggests "false";\n' > "$APT_CONF"
printf 'Package: snapd\nPin: release *\nPin-Priority: -10\n' > "$SNAP_PIN"
rm -rf /var/lib/snapd /var/cache/snapd /snap 2>/dev/null || true

update-grub
apt-get clean

echo
echo "-- check"
ok=1
systemctl is-active ssh systemd-networkd systemd-resolved || ok=0
initrd=/boot/initrd.img-$running
if command -v lsinitrd >/dev/null && lsinitrd "$initrd" >/dev/null 2>&1 \
   && lsinitrd "$initrd" | grep -q "lib/modules/$running/"; then
  echo "initrd for $running: readable, has its modules"
else
  ok=0
  echo "initrd for $running looks wrong; the original is at $STATE/initrd.img-$running.$ts"
  echo "restore before rebooting: cp $STATE/initrd.img-$running.$ts $initrd"
fi
grep -c '^menuentry\|submenu' /boot/grub/grub.cfg >/dev/null && echo "grub entries: $(grep -c "linux.*/vmlinuz-" /boot/grub/grub.cfg)"
grep -o 'crashkernel=[^ ]*' /boot/grub/grub.cfg | head -1 || echo "crashkernel: gone from grub.cfg"
df -h / | tail -1
echo "removed $(wc -l < "$STATE/removed.txt") packages (list in $STATE/removed.txt)"
if [[ $ok == 1 ]]; then echo "DONE: safe to reboot"; else echo "DONE WITH WARNINGS: read the check above before rebooting"; fi

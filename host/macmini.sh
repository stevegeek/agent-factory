#!/usr/bin/env bash
# Mac mini as an always-on factory host (Ubuntu 26.04). Run as root on the host:
#   sudo FACTORY_NAME=hub-factory bash macmini.sh          # apply (safe to re-run)
#   sudo FACTORY_NAME=hub-factory bash macmini.sh --undo   # remove what was written here
# Optional: FACTORY_NAME (default agent-factory, [a-z0-9-]; names the files written here).
# Only Macmini6,1 (late 2012) and Macmini8,1 (2018, T2) are accepted.
#
# Does:
#   1. reboot 10 s after a panic; soft lockups panic, and so do oopses, but only when the
#      full kernel logs of this boot and the previous one are clean (no reboot loop)
#   2. hardware watchdog (iTCO_wdt), only if the chipset lets it reboot the machine:
#      systemd pets it and reboots on a hang; a hung shutdown is cut off at 10 min
#   3. masks sleep, suspend, hibernate and hybrid-sleep
#   4. Wi-Fi and Bluetooth drivers blacklisted (gone after the next reboot)
#   5. Wake-on-LAN (magic packet) on the NIC that carries the default route, set at every boot
#   6. Macmini6,1: powers on again after AC loss (HM77 AFTERG3_EN, cleared at every boot)
#   7. Macmini8,1: Thunderbolt driver blacklisted (USB-C/TB3 ports likely stop working);
#      power-on after AC loss cannot be set from Linux on the T2 (a note is printed)
# The initramfs is rebuilt (update-initramfs, backed up first, checked after) only when it
# holds a module blacklisted here. Missing tools stop the script before anything is written; a step that
# fails later is a warning, the others still run, and a rerun completes it.
# Nothing is fetched or installed.
set -euo pipefail

NAME=${FACTORY_NAME:-agent-factory}
PANIC_CONF=/etc/sysctl.d/90-$NAME-panic.conf
WDT_UNIT=$NAME-watchdog-module.service
WDT_SERVICE=/etc/systemd/system/$WDT_UNIT
WDT_DROPIN=/etc/systemd/system.conf.d/90-$NAME-watchdog.conf
RADIOS_CONF=/etc/modprobe.d/$NAME-radios.conf
TB_CONF=/etc/modprobe.d/$NAME-thunderbolt.conf
WOL_UNIT=$NAME-wol.service
WOL_SERVICE=/etc/systemd/system/$WOL_UNIT
OLD_WOL_LINK=/etc/systemd/network/50-$NAME-wol.link
G3_UNIT=$NAME-poweron-after-g3.service
G3_SERVICE=/etc/systemd/system/$G3_UNIT
STATE=/var/lib/agent-factory-$NAME
A4_ORIG=$STATE/macmini-a4.orig
SLEEP_TARGETS=(sleep.target suspend.target hibernate.target hybrid-sleep.target)
RADIO_MODULES=(b43 bcma ssb brcmfmac brcmutil btusb bluetooth hci_uart)
TB_MODULES=(thunderbolt)
LPC=00:1f.0
HM77_ID=8086:1e57

log() { printf '\n== %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
failed=()
warn() { printf 'WARNING: %s\n' "$*" >&2; failed+=("$*"); }

[[ $NAME =~ ^[a-z0-9-]+$ ]] || die "FACTORY_NAME must match ^[a-z0-9-]+\$ (got: $NAME)"
[[ $EUID -eq 0 ]] || die "run with sudo"
. /etc/os-release
[[ ${ID:-} == ubuntu ]] || die "expected Ubuntu, found ${ID:-unknown}"
model=$(cat /sys/class/dmi/id/product_name 2>/dev/null || true)
case $model in
  Macmini6,1|Macmini8,1) ;;
  *) die "not a Macmini6,1 or Macmini8,1 (product_name: ${model:-unknown})" ;;
esac
command -v update-initramfs >/dev/null || die "update-initramfs not found (Ubuntu 26.04: from the dracut package)"
command -v lsinitrd >/dev/null || die "lsinitrd not found (from the dracut package)"

changed=()
reboot_for=()

# Writes $2 to file $1 only if the content differs. Returns 0 when changed.
write_if_changed() {
  local path=$1 content=$2
  if [[ -f $path ]] && [[ $(cat "$path") == "$content" ]]; then
    return 1
  fi
  mkdir -p "$(dirname "$path")"
  printf '%s\n' "$content" > "$path"
  chmod 644 "$path"
  return 0
}

# The device (lspci -s) is the HM77 LPC bridge, whose GEN_PMCON_3 bit 0 is AFTERG3_EN.
is_hm77() {
  command -v lspci >/dev/null && [[ $(lspci -s "$LPC" -n 2>/dev/null) == *" $HM77_ID"* ]]
}

# Prints the listing of initramfs $1 (root only: the images are 0600). Captured first:
# under pipefail, an early-exiting grep would fail lsinitrd with SIGPIPE.
initrd_listing() { lsinitrd "$1" 2>/dev/null; }

# Prints which of the modules $2... are in listing $1.
modules_in() {
  local listing=$1 m hits=""; shift
  for m in "$@"; do
    grep -qE "/$m\.ko(\.(zst|xz|gz))?\$" <<<"$listing" && hits+="$m "
  done
  printf '%s' "$hits"
}

# Checks a freshly built image: lsinitrd reads it, and it holds device-mapper, the lvm
# binary and the kernel's modules (what this host needs to find /).
initrd_ok() {
  local img=$1 kver=$2 listing
  listing=$(lsinitrd "$img" 2>/dev/null) || { echo "lsinitrd cannot read it"; return 1; }
  grep -qE '/dm[-_]mod\.ko(\.(zst|xz|gz))?$' <<<"$listing" || { echo "no dm-mod module"; return 1; }
  grep -qE '(^|[ /])(usr/)?s?bin/lvm$' <<<"$listing" || { echo "no lvm binary"; return 1; }
  grep -qE "(^|[ /])(usr/)?lib/modules/$kver/" <<<"$listing" || { echo "no lib/modules/$kver/"; return 1; }
}

# Rebuilds each image REBUILD_WHY (image -> reason) names: checks /boot has 3x their size
# free, backs the image up once, runs update-initramfs -u -k <kver> (what kernel postinst
# runs), checks the result and drops the backup. Returns 1 on failure, after saying how
# to restore.
declare -A REBUILD_WHY=()
rebuild_initramfs() {
  local img kver need=0 free why
  [[ ${#REBUILD_WHY[@]} -gt 0 ]] || { echo "initramfs: nothing in it to change"; return 0; }
  for img in "${!REBUILD_WHY[@]}"; do
    echo "initramfs $img: ${REBUILD_WHY[$img]}"
    need=$(( need + 3 * $(stat -c %s "$img") ))
  done
  free=$(df -B1 --output=avail /boot | tail -1)
  if (( free < need )); then
    echo "ERROR: /boot has $(( free / 1048576 )) MB free, needs $(( need / 1048576 )) MB (3x the images); initramfs not rebuilt" >&2
    return 1
  fi
  for img in "${!REBUILD_WHY[@]}"; do
    kver=${img#/boot/initrd.img-}
    if [[ ! -e $img.pre-$NAME ]]; then
      cp -a "$img" "$img.pre-$NAME" || { echo "ERROR: cannot back up $img; not rebuilt" >&2; return 1; }
      echo "backup: $img.pre-$NAME"
    fi
    if ! update-initramfs -u -k "$kver"; then
      echo "ERROR: update-initramfs -u -k $kver failed. Before rebooting, restore the previous image:" >&2
      echo "  sudo cp -a $img.pre-$NAME $img" >&2
      return 1
    fi
    if ! why=$(initrd_ok "$img" "$kver"); then
      echo "ERROR: the rebuilt $img looks wrong ($why). Before rebooting, restore the previous image:" >&2
      echo "  sudo cp -a $img.pre-$NAME $img" >&2
      return 1
    fi
    rm -f "$img.pre-$NAME"
    echo "$img rebuilt and checked (dm-mod, lvm, modules); removed $img.pre-$NAME"
  done
}

undo() {
  log "Removing $NAME Mac mini config"
  if [[ -f $G3_SERVICE ]]; then
    systemctl disable --now "$G3_UNIT" 2>/dev/null || true
    rm -f "$G3_SERVICE"
    echo "removed $G3_SERVICE"
  fi
  if [[ $model == Macmini6,1 ]] && command -v setpci >/dev/null && is_hm77; then
    bit=1   # firmware default: stay off after AC loss
    if [[ -s $A4_ORIG ]] && orig=$(<"$A4_ORIG") && [[ $orig =~ ^[0-9a-fA-F]{2}$ ]]; then
      bit=$(( 16#$orig & 1 ))
    fi
    if setpci -s "$LPC" "0xa4.b=$bit:1"; then
      echo "AFTERG3_EN back to $bit: 0xa4=$(setpci -s "$LPC" 0xa4.b)"
    else
      warn "could not restore AFTERG3_EN"
    fi
  fi
  rm -f "$A4_ORIG"
  rmdir "$STATE" 2>/dev/null || true
  if [[ -f $WOL_SERVICE ]]; then
    systemctl disable --now "$WOL_UNIT" 2>/dev/null || true
    rm -f "$WOL_SERVICE"
    echo "removed $WOL_SERVICE (the NIC keeps its current Wake-on setting until reboot)"
  fi
  if [[ -f $OLD_WOL_LINK ]]; then rm -f "$OLD_WOL_LINK"; echo "removed $OLD_WOL_LINK"; fi
  if [[ -f $WDT_SERVICE ]]; then
    systemctl disable --now "$WDT_UNIT" 2>/dev/null || true
    rm -f "$WDT_SERVICE"
    echo "removed $WDT_SERVICE"
  fi
  if [[ -f $WDT_DROPIN ]]; then
    rm -f "$WDT_DROPIN"
    rmdir /etc/systemd/system.conf.d 2>/dev/null || true
    echo "removed $WDT_DROPIN"
  fi
  # PID1 drops (and disarms) the watchdog once it re-executes without the drop-in
  systemctl daemon-reexec || warn "systemctl daemon-reexec failed"
  modprobe -r iTCO_wdt 2>/dev/null || true
  removed_mods=()
  if [[ -f $RADIOS_CONF ]]; then rm -f "$RADIOS_CONF"; echo "removed $RADIOS_CONF"; removed_mods+=("${RADIO_MODULES[@]}"); fi
  if [[ -f $TB_CONF ]]; then rm -f "$TB_CONF"; echo "removed $TB_CONF"; removed_mods+=("${TB_MODULES[@]}"); fi
  systemctl unmask "${SLEEP_TARGETS[@]}" || warn "unmasking the sleep targets failed"
  echo "unmasked ${SLEEP_TARGETS[*]}"
  if [[ -f $PANIC_CONF ]]; then rm -f "$PANIC_CONF"; echo "removed $PANIC_CONF"; fi
  # back to the kernel defaults, then whatever other sysctl.d files set
  sysctl -q kernel.panic=0 kernel.panic_on_oops=0 kernel.softlockup_panic=0
  sysctl --system >/dev/null || warn "sysctl --system failed"
  echo "now: panic=$(sysctl -n kernel.panic) panic_on_oops=$(sysctl -n kernel.panic_on_oops) softlockup_panic=$(sysctl -n kernel.softlockup_panic)"
  systemctl daemon-reload

  log "initramfs"
  for img in /boot/initrd.img-*; do
    [[ -f $img && $img != *.pre-* ]] || continue
    listing=$(initrd_listing "$img") || { warn "lsinitrd cannot read $img"; continue; }
    if grep -qE "etc/modprobe\.d/$NAME-(radios|thunderbolt)\.conf\$" <<<"$listing"; then
      REBUILD_WHY[$img]="still carries this script's blacklist"
    elif [[ ${#removed_mods[@]} -gt 0 ]] && hits=$(modules_in "$listing" "${removed_mods[@]}") && [[ -n $hits ]]; then
      REBUILD_WHY[$img]="holds ${hits% } (blacklist removed)"
    fi
  done
  if rebuild_initramfs; then
    for f in /boot/initrd.img-*.pre-"$NAME"; do
      [[ -e $f ]] || continue
      rm -f "$f"; echo "removed leftover backup $f"
    done
  else
    warn "initramfs rebuild failed (see above); backups under /boot/*.pre-$NAME kept"
  fi
  [[ ${#removed_mods[@]} -eq 0 ]] || echo "reboot to bring the Wi-Fi/Bluetooth (and Thunderbolt) drivers back"
  echo
  if [[ ${#failed[@]} -gt 0 ]]; then
    printf 'Done with warnings:\n'; printf '  - %s\n' "${failed[@]}"; exit 1
  fi
  echo "Done."
  exit 0
}

[[ ${1:-} == --undo ]] && undo
[[ $# -eq 0 ]] || die "usage: macmini.sh [--undo]"

if [[ $model == Macmini6,1 ]]; then
  command -v setpci >/dev/null && command -v lspci >/dev/null || die "setpci/lspci missing: apt install pciutils"
fi
echo "model: $model"

log "Reboot on panic and soft lockup; on oops only after two clean boots"
oops_line="kernel.panic_on_oops = 1"
oops_why=""
# a boot counts as checked only if its kernel log still starts at "Linux version"
for b in 0 -1; do
  if ! klog=$(journalctl -k -b "$b" --no-pager 2>/dev/null) || ! grep -q 'Linux version' <<<"$klog"; then
    oops_why+="boot $b: kernel log missing or incomplete (no persistent journal, rotated, or unreadable)"$'\n'
    continue
  fi
  hit=$(grep -E 'BUG:|Oops' <<<"$klog" | head -1 || true)
  [[ -z $hit ]] || oops_why+="boot $b: $hit"$'\n'
done
if [[ -n $oops_why ]]; then
  printf 'not setting panic_on_oops (a recurring oops would reboot forever):\n%s' "$oops_why"
  echo "rerun once this boot and the previous one both have a full, clean kernel log"
  oops_line="# kernel.panic_on_oops not set: the last two boots' kernel logs were not both complete and clean"
fi
if write_if_changed "$PANIC_CONF" "# $NAME: an unattended host reboots instead of hanging
kernel.panic = 10
$oops_line
kernel.softlockup_panic = 1"; then
  changed+=("$PANIC_CONF")
fi
[[ -z $oops_why ]] || sysctl -q kernel.panic_on_oops=0
sysctl -p "$PANIC_CONF" || warn "sysctl -p $PANIC_CONF failed"

log "Hardware watchdog (iTCO_wdt)"
# Ubuntu blacklists every watchdog driver, and systemd-modules-load honours that
# blacklist, so a modules-load.d entry would be skipped: a unit runs modprobe instead
# (an explicit modprobe ignores the blacklist). PID1 opens /dev/watchdog0 when it appears.
wdt_why=""
if ! modprobe iTCO_wdt 2>/dev/null; then
  wdt_why="modprobe iTCO_wdt failed"
elif [[ ! -e /dev/watchdog0 ]]; then
  wdt_why="/dev/watchdog0 did not appear"
elif [[ $(cat /sys/class/watchdog/watchdog0/identity 2>/dev/null) != *iTCO* ]]; then
  wdt_why="watchdog0 is $(cat /sys/class/watchdog/watchdog0/identity 2>/dev/null || echo unknown), not iTCO"
else
  kmsg=$(journalctl -k -b --no-pager 2>/dev/null || dmesg 2>/dev/null || true)
  hit=$(grep -iE 'iTCO.*(NO_REBOOT|disabled by hardware)' <<<"$kmsg" | tail -1 || true)
  [[ -z $hit ]] || wdt_why="the firmware locks it out: $hit"
fi
if [[ -n $wdt_why ]]; then
  echo "skipping the watchdog: $wdt_why"
  modprobe -r iTCO_wdt 2>/dev/null || true
else
  wdt_changed=0
  if write_if_changed "$WDT_SERVICE" "# $NAME: load the hardware watchdog despite Ubuntu's watchdog blacklist
[Unit]
Description=$NAME: load the iTCO hardware watchdog
DefaultDependencies=no
Before=sysinit.target

[Service]
Type=oneshot
ExecStart=$(command -v modprobe) iTCO_wdt
RemainAfterExit=yes

[Install]
WantedBy=sysinit.target"; then
    changed+=("$WDT_SERVICE"); wdt_changed=1
  fi
  if write_if_changed "$WDT_DROPIN" "# $NAME: reboot if PID1 stops petting the watchdog, or a shutdown hangs
[Manager]
RuntimeWatchdogSec=60s
RebootWatchdogSec=10min"; then
    changed+=("$WDT_DROPIN"); wdt_changed=1
  fi
  systemctl daemon-reload
  systemctl enable "$WDT_UNIT" || warn "enabling $WDT_UNIT failed"
  if [[ $wdt_changed == 1 ]] || [[ $(systemctl show -P RuntimeWatchdogUSec) == 0 ]]; then
    systemctl daemon-reexec || warn "systemctl daemon-reexec failed"
  fi
fi
systemctl show -p RuntimeWatchdogUSec

log "No sleep"
systemctl mask "${SLEEP_TARGETS[@]}" || warn "masking the sleep targets failed"

log "Wi-Fi and Bluetooth drivers off (after the next reboot)"
blacklisted=("${RADIO_MODULES[@]}")
confs=("$RADIOS_CONF")
if write_if_changed "$RADIOS_CONF" "# $NAME: no Wi-Fi or Bluetooth on a wired server
$(printf 'blacklist %s\n' "${RADIO_MODULES[@]}")"; then
  changed+=("$RADIOS_CONF"); reboot_for+=("Wi-Fi/Bluetooth drivers")
fi
echo "loaded now (left alone until reboot): $(lsmod | awk 'NR>1{print $1}' | grep -xE "$(IFS='|'; echo "${RADIO_MODULES[*]}")" | tr '\n' ' ' || true)"

if [[ $model == Macmini8,1 ]]; then
  log "Thunderbolt off (after the next reboot)"
  blacklisted+=("${TB_MODULES[@]}")
  confs+=("$TB_CONF")
  if write_if_changed "$TB_CONF" "# $NAME: no Thunderbolt (the USB-C/TB3 ports likely stop working)
blacklist thunderbolt
install thunderbolt /bin/false"; then
    changed+=("$TB_CONF"); reboot_for+=("Thunderbolt driver")
  fi
  echo "note: the USB-C/Thunderbolt 3 ports likely stop working; USB-A and HDMI are unaffected"
fi

log "initramfs"
# Rebuilt only when an image holds a blacklisted module and lacks our blacklist, or a
# blacklist file is newer than the image (it changed, or an earlier rebuild failed).
for img in /boot/initrd.img-*; do
  [[ -f $img && $img != *.pre-* ]] || continue
  listing=$(initrd_listing "$img") || { warn "lsinitrd cannot read $img"; continue; }
  why=""
  for c in "${confs[@]}"; do
    [[ $c -nt $img ]] && why+="$(basename "$c") is newer; "
  done
  hits=$(modules_in "$listing" "${blacklisted[@]}")
  if [[ -n $hits ]]; then
    for c in "${confs[@]}"; do
      grep -qE "${c#/}\$" <<<"$listing" || { why+="holds ${hits% } without $(basename "$c"); "; break; }
    done
  fi
  [[ -z $why ]] || REBUILD_WHY[$img]=${why%; }
done
rebuild_initramfs || warn "initramfs rebuild failed (see above); a rerun retries it"

log "Wake-on-LAN (magic packet)"
if [[ -f $OLD_WOL_LINK ]]; then rm -f "$OLD_WOL_LINK"; echo "removed the old $OLD_WOL_LINK"; fi
wol_if=$(ip -o route show default 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i+1); exit}}' || true)
ethtool=$(command -v ethtool || true)
supports=""
[[ -z $ethtool || -z $wol_if ]] || supports=$("$ethtool" "$wol_if" 2>/dev/null | awk -F': *' '/Supports Wake-on/{print $2}' || true)
if [[ -z $wol_if ]]; then
  warn "Wake-on-LAN skipped: no default route"
elif [[ ! -e /sys/class/net/$wol_if/device ]]; then
  warn "Wake-on-LAN skipped: $wol_if (default route) is not a physical NIC"
elif [[ -z $ethtool ]]; then
  warn "Wake-on-LAN skipped: ethtool is not installed (apt install ethtool, then rerun)"
elif [[ $supports != *g* ]]; then
  echo "skipping Wake-on-LAN: $wol_if does not support magic-packet wake (Supports Wake-on: ${supports:-unknown})"
else
  dev_unit=$(systemd-escape -p --suffix=device "/sys/subsystem/net/devices/$wol_if")
  if write_if_changed "$WOL_SERVICE" "# $NAME: Wake-on-LAN (magic packet) on $wol_if, set at every boot
[Unit]
Description=$NAME: Wake-on-LAN on $wol_if
BindsTo=$dev_unit
After=$dev_unit

[Service]
Type=oneshot
ExecStart=$ethtool -s $wol_if wol g
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target"; then
    changed+=("$WOL_SERVICE")
    systemctl daemon-reload
    systemctl stop "$WOL_UNIT" 2>/dev/null || true   # RemainAfterExit: rerun it with the new line
  fi
  systemctl enable --now "$WOL_UNIT" || warn "$WOL_UNIT failed: systemctl status $WOL_UNIT"
  echo "$wol_if: $("$ethtool" "$wol_if" 2>/dev/null | grep -E '^\s*Wake-on:' | tr -s ' \t' ' ' || echo 'Wake-on: unknown')"
  echo "device wakeup: $(cat "/sys/class/net/$wol_if/device/power/wakeup" 2>/dev/null || echo unknown)"
fi

if [[ $model == Macmini6,1 ]]; then
  log "Power on after AC loss (HM77 AFTERG3_EN)"
  if ! is_hm77; then
    echo "skipping: $LPC is not the HM77 LPC bridge ($HM77_ID): $(lspci -s "$LPC" -n 2>/dev/null || echo absent)"
  elif ! before=$(setpci -s "$LPC" 0xa4.b); then
    warn "cannot read $LPC 0xa4; power-on after AC loss not set"
  else
    echo "0xa4 before: $before"
    if [[ ! -s $A4_ORIG ]]; then
      install -d -m 755 "$STATE"
      printf '%s\n' "$before" > "$A4_ORIG"
      echo "original saved to $A4_ORIG"
    fi
    if write_if_changed "$G3_SERVICE" "# $NAME: clear AFTERG3_EN (bit 0 of GEN_PMCON_3) so the Mac mini powers on after AC loss.
# The firmware sets it again at every boot, so this runs at every boot.
[Unit]
Description=$NAME: power on after AC loss

[Service]
Type=oneshot
ExecStart=$(command -v setpci) -s $LPC 0xa4.b=0:1

[Install]
WantedBy=multi-user.target"; then
      changed+=("$G3_SERVICE")
      systemctl daemon-reload
    fi
    systemctl enable "$G3_UNIT" || warn "enabling $G3_UNIT failed"
    systemctl start "$G3_UNIT" || warn "$G3_UNIT failed: systemctl status $G3_UNIT"
    after=$(setpci -s "$LPC" 0xa4.b || echo ff)
    echo "0xa4 after:  $after"
    (( (16#$after & 1) == 0 )) || warn "AFTERG3_EN (bit 0) is still set after $G3_UNIT"
  fi
fi

if [[ $model == Macmini8,1 ]]; then
  log "Power on after AC loss"
  echo "Not settable from Linux on a T2 Mac. Test it by pulling the plug; if it stays off, boot"
  echo "macOS Recovery, open Terminal and run: pmset autorestart 1"
fi

log "Verification"
set +e
echo "-- sysctl";   sysctl kernel.panic kernel.panic_on_oops kernel.softlockup_panic
echo "-- watchdog"; systemctl show -p RuntimeWatchdogUSec -p RebootWatchdogUSec
echo "-- sleep";    systemctl is-enabled "${SLEEP_TARGETS[@]}" | paste -sd' '
set -e

cat <<EOF

Changed: ${changed[*]:-nothing}
Reboot needed for: ${reboot_for[*]:-nothing}
Manual tests:
  - power loss: pull the plug for 15 s, plug it back in; the Mac should boot by itself.
  - crash (only if you want it): echo c > /proc/sysrq-trigger panics the kernel, and the host
    reboots 10 s later through kernel.panic; that tests the panic setting, not the watchdog.
    A watchdog test needs a real hang, so none is suggested.
If it ever reboot-loops: at the GRUB menu (hold Shift or Esc while it boots), press e, add
  systemd.mask=systemd-sysctl.service to the end of the linux line, boot with Ctrl-X, then run:
  sudo FACTORY_NAME=$NAME bash macmini.sh --undo
EOF
if [[ ${#failed[@]} -gt 0 ]]; then
  printf '\nDone with warnings (rerun to complete):\n'; printf '  - %s\n' "${failed[@]}"
  exit 1
fi

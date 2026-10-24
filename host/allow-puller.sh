#!/usr/bin/env bash
# Let a pull host (transport = "pull", such as a desktop running factory-pull) call
# in to this dispatcher. Run as root here, after setup.sh and a deploy (so
# ~factory/factory/bin/factory-remote exists):
#   sudo bash allow-puller.sh --host desktop --from 192.0.2.10 --key 'ssh-ed25519 AAAA... factory-pull'
#   sudo bash allow-puller.sh --undo --host desktop
# Optional: FACTORY_USER (default factory).
#
# Keeps one line per pull host in /etc/ssh/authorized_keys/<user>.pullers (root-owned;
# setup.sh lists it in AuthorizedKeysFile and never writes it, so a re-run of setup.sh
# keeps it): the key works only from that address, can only run
# `factory-remote mailbox <host>`, and gets no pty or forwarding (restrict). Re-running
# for a host replaces its line; other hosts' lines are kept. --undo removes only that
# host's line, and the file once it is empty. Nothing is reloaded: sshd reads the file
# at each login.
set -euo pipefail

FACTORY_USER=${FACTORY_USER:-factory}
KEYS=/etc/ssh/authorized_keys/$FACTORY_USER.pullers

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "run with sudo"
[[ $FACTORY_USER =~ ^[a-z_][a-z0-9_-]*$ ]] || die "FACTORY_USER is not a user name: $FACTORY_USER"

undo= host= from= key=
while [[ $# -gt 0 ]]; do
  case $1 in
    --undo) undo=1; shift ;;
    --host) [[ $# -ge 2 ]] || die "--host needs a value"; host=$2; shift 2 ;;
    --from) [[ $# -ge 2 ]] || die "--from needs a value"; from=$2; shift 2 ;;
    --key) [[ $# -ge 2 ]] || die "--key needs a value"; key=$2; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[[ $host =~ ^[a-z0-9-]+$ ]] || die "--host must match ^[a-z0-9-]+\$ (the [hosts.<name>] of the pull host)"

home=$(getent passwd "$FACTORY_USER" | cut -d: -f6)
[[ -n $home ]] || die "user $FACTORY_USER not found"
cmd=$home/factory/bin/factory-remote
[[ $cmd != *[[:space:]\"]* ]] || die "$cmd contains a space or quote"
# the forced command that marks this host's line
mark=" mailbox $host\" "

if [[ -e $KEYS || -L $KEYS ]]; then
  [[ -f $KEYS && ! -L $KEYS ]] || die "$KEYS is not a regular file"
fi

# Writes stdin to $KEYS atomically (root:root 0644), or removes $KEYS when stdin is empty.
replace_keys() {
  local tmp
  install -d -m 755 -o root -g root "$(dirname "$KEYS")"
  tmp=$(mktemp "$KEYS.XXXXXX")
  # a failed write (set -e) leaves no temporary file beside the live one
  # (expanded now: a local is gone by the time the EXIT trap runs)
  trap "rm -f -- $(printf '%q' "$tmp")" EXIT
  cat > "$tmp"
  if [[ ! -s $tmp ]]; then
    rm -f "$tmp" "$KEYS"
    trap - EXIT
    echo "removed $KEYS (no pull host left)"
    return
  fi
  chown root:root "$tmp"
  chmod 644 "$tmp"
  mv -f "$tmp" "$KEYS"
  trap - EXIT
  echo "wrote $KEYS:"
  cat "$KEYS"
}

# Every line of $KEYS except this host's.
others() {
  [[ -f $KEYS ]] || return 0
  grep -vF -- "$mark" "$KEYS" || true
}

if [[ -n $undo ]]; then
  [[ -z $from && -z $key ]] || die "--undo takes only --host"
  if [[ ! -f $KEYS ]] || ! grep -qF -- "$mark" "$KEYS"; then
    echo "no line for $host in $KEYS; nothing to do"
    exit 0
  fi
  others | replace_keys
  exit 0
fi

[[ $from =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "--from must be one IPv4 address"
IFS=. read -r -a octets <<<"$from"
for o in "${octets[@]}"; do (( 10#$o <= 255 )) || die "--from must be one IPv4 address"; done
[[ $key =~ ^ssh-ed25519\ [A-Za-z0-9+/]+={0,2}(\ [A-Za-z0-9@._-]+)?$ ]] || die "--key must be one ssh-ed25519 public key"
[[ -x $cmd ]] || die "$cmd missing: deploy to this host first"

# sshd -T prints AuthorizedKeysFile with %u unexpanded
effective=$(sshd -T -C "user=$FACTORY_USER,host=localhost,addr=127.0.0.1" | grep -i '^authorizedkeysfile ' || true)
grep -qE "(^| )($(dirname "$KEYS")/%u\.pullers|$KEYS)( |$)" <<<"$effective" \
  || die "sshd does not read $KEYS for $FACTORY_USER (have: ${effective:-nothing}); re-run the current setup.sh first"

{
  others
  printf 'restrict,from="%s",command="%s mailbox %s" %s\n' "$from" "$cmd" "$host" "$key"
} | replace_keys

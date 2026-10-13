#!/usr/bin/env bash
# Let a dispatcher host run Pi jobs on this helper host. Run as root here, after
# setup.sh and a deploy (so ~factory/factory/bin/factory-remote exists):
#   sudo bash allow-dispatcher.sh --from 192.168.0.48 --key 'ssh-ed25519 AAAA... comment'
#   sudo bash allow-dispatcher.sh --undo
# Optional: FACTORY_USER (default factory).
#
# Writes one line to /etc/ssh/authorized_keys/<user>.dispatchers (root-owned; setup.sh
# lists it in AuthorizedKeysFile): the key works only from that address, can only
# run factory-remote, and gets no pty or forwarding (restrict). Re-running replaces
# the line. Nothing is reloaded: sshd reads the file at each login.
set -euo pipefail

FACTORY_USER=${FACTORY_USER:-factory}
KEYS=/etc/ssh/authorized_keys/$FACTORY_USER.dispatchers

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "run with sudo"

if [[ ${1:-} == --undo ]]; then
  rm -f "$KEYS"
  echo "removed $KEYS"
  exit 0
fi

from= key=
while [[ $# -gt 0 ]]; do
  case $1 in
    --from) from=$2; shift 2 ;;
    --key) key=$2; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[[ $from =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "--from must be one IPv4 address"
IFS=. read -r -a octets <<<"$from"
for o in "${octets[@]}"; do (( 10#$o <= 255 )) || die "--from must be one IPv4 address"; done
[[ $key =~ ^ssh-ed25519\ [A-Za-z0-9+/]+={0,2}(\ [A-Za-z0-9@._-]+)?$ ]] || die "--key must be one ssh-ed25519 public key"

home=$(getent passwd "$FACTORY_USER" | cut -d: -f6)
[[ -n $home ]] || die "user $FACTORY_USER not found"
cmd=$home/factory/bin/factory-remote
[[ -x $cmd ]] || die "$cmd missing: deploy to this host first"

[[ $cmd != *[[:space:]\"]* ]] || die "$cmd contains a space or quote"

# sshd -T prints AuthorizedKeysFile with %u unexpanded
effective=$(sshd -T -C "user=$FACTORY_USER,host=localhost,addr=127.0.0.1" | grep -i '^authorizedkeysfile ' || true)
grep -qE "(^| )($(dirname "$KEYS")/%u\.dispatchers|$KEYS)( |$)" <<<"$effective" \
  || die "sshd does not read $KEYS for $FACTORY_USER (have: ${effective:-nothing}); re-run the current setup.sh first"

install -d -m 755 -o root -g root "$(dirname "$KEYS")"
tmp=$(mktemp "$KEYS.XXXXXX")
printf 'restrict,from="%s",command="%s" %s\n' "$from" "$cmd" "$key" > "$tmp"
chown root:root "$tmp"
chmod 644 "$tmp"
mv -f "$tmp" "$KEYS"
echo "wrote $KEYS:"
cat "$KEYS"

#!/usr/bin/env bash
# Render from a config directory and copy to a host, as the factory user over SSH.
#   deploy.sh --config DIR --host SSH_HOST [--dry-run]
# Replaces the host's config.toml (previous kept as config.toml.prev); never touches
# the board or logs. --dry-run renders only.
set -euo pipefail
core=$(cd "$(dirname "$0")" && pwd)
config= host= dry=
while [[ $# -gt 0 ]]; do
  case $1 in
    --config) config=$2; shift 2 ;;
    --host) host=$2; shift 2 ;;
    --dry-run) dry=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ -n $config && ( -n $host || -n $dry ) ]] || { sed -n 2,4p "$0"; exit 2; }

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
python3 "$core/bin/render" "$config" "$out/r" >/dev/null
r=$out/r
if [[ -n $dry ]]; then
  (cd "$r" && find . -type f | sort)
  trap - EXIT
  echo "rendered into $r"
  exit 0
fi

ssh "$host" 'mkdir -p ~/factory/bin ~/factory/systemd ~/factory/images ~/factory/board/tickets ~/factory/logs ~/factory/gate ~/.local/bin ~/.scion/templates'
scp -q "$r"/bin/* "$host":factory/bin/
scp -q "$r"/systemd/* "$host":factory/systemd/
scp -q -r "$r"/images/* "$host":factory/images/
scp -q "$r/patch_provision.py" "$host":factory/patch_provision.py
ssh "$host" 'chmod 755 ~/factory/bin/*; ln -sf ~/factory/bin/factory ~/.local/bin/factory; ln -sf ~/factory/bin/pi-run ~/.local/bin/pi-run'
ssh "$host" 'test -e ~/factory/config.toml && cp ~/factory/config.toml ~/factory/config.toml.prev || true'
scp -q "$r/config.toml" "$host":factory/config.toml
for t in "$r"/scion-templates/*/; do
  name=$(basename "$t")
  ssh "$host" "mkdir -p ~/.scion/templates/$name"
  scp -q "$t"* "$host":.scion/templates/"$name"/
done
echo "deployed to $host"

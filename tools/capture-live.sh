#!/usr/bin/env bash
# Capture a private snapshot of the live factory's board for the live-board tests, which read
# it from FACTORY_LIVE_SNAPSHOT and skip without it (jsonx, ticket, ops).
#
# The snapshot holds real project names, issue text and config: write it into the private
# config repository, never into this public one. The script
# refuses a destination inside the repository it lives in.
#
#   tools/capture-live.sh [-H SSH_TARGET] [-r REMOTE_ROOT] DEST
#
#   SSH_TARGET   the factory host's ssh target (default: $FACTORY_SSH)
#   REMOTE_ROOT  the factory root on that host, relative to its home (default: factory)
#
# What it copies, read-only, from the host: board/ (without lock files), config.toml,
# state.json, watches.json and pauses.json when they exist, memory/ (the bare memory
# stores, as files), the last 2000 lines of logs/factory.log, plus memory-listing.txt:
# each memory store's head and the names and sizes of its files. It never copies the
# token store, the GitHub App key, the Claude token or Foreman's secrets. Values of config
# keys named like a secret, token or password are replaced by "<scrubbed>".
set -euo pipefail

target="${FACTORY_SSH:-}"
root="factory"
while getopts "H:r:h" opt; do
	case "$opt" in
	H) target="$OPTARG" ;;
	r) root="$OPTARG" ;;
	h | *)
		sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'
		[ "$opt" = h ] && exit 0 || exit 2
		;;
	esac
done
shift $((OPTIND - 1))
[ $# -eq 1 ] || { echo "usage: $0 [-H SSH_TARGET] [-r REMOTE_ROOT] DEST" >&2; exit 2; }
[ -n "$target" ] || { echo "$0: no host: pass -H or set FACTORY_SSH" >&2; exit 2; }
case "$root" in
/* | *..* | *[!A-Za-z0-9._/-]*) echo "$0: REMOTE_ROOT is a plain path under the host's home" >&2; exit 2 ;;
esac

umask 077
dest="$(realpath -m -- "$1")"
here="$(cd "$(dirname "$0")/.." && pwd -P)"
case "$dest/" in
"$here"/*) echo "$0: $dest is inside $here, a public repository; capture into a directory outside it" >&2; exit 2 ;;
esac
mkdir -p "$dest"

work="$(mktemp -d "$dest/.capture.XXXXXX")"
trap 'rm -rf "$work"' EXIT

# One read-only pass on the host: a tar of the board and state files on stdout.
ssh -o BatchMode=yes "$target" bash -s -- "$root" >"$work/live.tar" <<'REMOTE'
set -euo pipefail
cd "$HOME/$1"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
if [ -f logs/factory.log ]; then
	mkdir "$tmp/logs"
	tail -n 2000 logs/factory.log >"$tmp/logs/factory.log"
fi
{
	for f in board config.toml state.json watches.json pauses.json memory; do
		[ -e "$f" ] && printf '%s\n' "$f"
	done
} | tar -cf - --exclude='*.lock' --exclude='.locks' --exclude='*.tmp' -T - \
	$([ -d "$tmp/logs" ] && printf -- '-C %s logs' "$tmp")
REMOTE

ssh -o BatchMode=yes "$target" bash -s -- "$root" >"$work/memory-listing.txt" <<'REMOTE'
set -euo pipefail
cd "$HOME/$1"
[ -d memory ] || exit 0
for store in memory/*/*.git; do
	[ -d "$store" ] || continue
	export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
	printf '== %s\n' "${store#memory/}"
	git --git-dir="$store" log -1 --format='head %H %cI' 2>/dev/null || echo "head none"
	git --git-dir="$store" ls-tree -r -l --full-tree HEAD 2>/dev/null | awk '{print $4, $5}' || true
done
REMOTE

mkdir "$work/snapshot"
tar -xf "$work/live.tar" -C "$work/snapshot" --no-same-owner --no-same-permissions
mv "$work/memory-listing.txt" "$work/snapshot/memory-listing.txt"
if [ -f "$work/snapshot/config.toml" ]; then
	sed -E -i 's/^([[:space:]]*[A-Za-z0-9_-]*(secret|token|password)[A-Za-z0-9_-]*[[:space:]]*=[[:space:]]*).*/\1"<scrubbed>"/' \
		"$work/snapshot/config.toml"
fi

# Replace the previous snapshot as a whole.
for f in board config.toml state.json watches.json pauses.json memory logs memory-listing.txt; do
	rm -rf "${dest:?}/$f"
	[ -e "$work/snapshot/$f" ] && mv "$work/snapshot/$f" "$dest/$f"
done
tickets=$(find "$dest/board/tickets" -maxdepth 1 -name 'T*.json' 2>/dev/null | wc -l)
echo "captured $tickets tickets into $dest; run the live-board tests with FACTORY_LIVE_SNAPSHOT=$dest go test ./internal/..."

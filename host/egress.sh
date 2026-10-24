#!/usr/bin/env bash
# Narrow the factory user's outbound traffic to the services it uses. Run as root on each host:
#   sudo bash egress.sh --log-only [--allow-ip 192.0.2.11]...   log what would be refused, refuse nothing
#   sudo bash egress.sh --enforce  [--allow-ip ...]                 refuse everything not allowed
#   sudo bash egress.sh --status                                    the rules, the sets, recent refusals
#   sudo bash egress.sh --undo                                      remove all of it
# Optional: FACTORY_USER (default factory), EXTRA_DOMAINS="a.example b.example".
#
# Only the factory user's traffic is filtered: its own uid and its containers' sub-uids (/etc/subuid),
# matched by `meta skuid` into a chain of its own. Everything else (root, apt, tailscaled, the kernel's
# own IPv6 neighbour discovery) never enters that chain. Rootless containers' traffic leaves through the
# user's own network helper, so it is covered. Domains are resolved into address sets by a root timer
# every 10 minutes; an address stays allowed for a day after it was last seen, so CDN round-robins settle
# in. Loopback, replies, and DNS to the system's own resolvers are allowed.
#
# Start with --log-only for a week and read `journalctl -k | grep factory-egress`; add what's missing to
# EXTRA_DOMAINS, then --enforce. Re-running empties the sets for a moment (the refresh fills them at once).
# Re-adding an element must reset its one-day timeout, or every address expires a day after install. Check
# once per host: `nft add element inet factory_egress allow4 '{ 192.0.2.1 timeout 1h }'`, add it again with
# `timeout 1d`, then `nft list set inet factory_egress allow4` must show it expiring in about a day.
set -euo pipefail

FACTORY_USER=${FACTORY_USER:-factory}
TABLE=factory_egress
REFRESH=/usr/local/sbin/factory-egress-refresh
CONF=/etc/factory-egress.conf
RULES=/etc/factory-egress.nft
SYSD=/etc/systemd/system
DOMAINS="api.anthropic.com claude.ai downloads.claude.ai statsig.anthropic.com console.anthropic.com platform.claude.com
mcp-proxy.anthropic.com storage.googleapis.com
github.com api.github.com codeload.github.com objects.githubusercontent.com raw.githubusercontent.com
release-assets.githubusercontent.com
rubygems.org index.rubygems.org registry.npmjs.org
docker.io registry-1.docker.io auth.docker.io production.cloudflare.docker.com
archive.ubuntu.com security.ubuntu.com ports.ubuntu.com
web.push.apple.com api.push.apple.com fcm.googleapis.com updates.push.services.mozilla.com"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "run with sudo"
command -v nft >/dev/null || die "nft not found (apt install nftables)"

units=(factory-egress-refresh.timer factory-egress-refresh.service factory-egress-table.service)
if [[ ${1:-} == --undo ]]; then
  for u in "${units[@]}"; do systemctl disable --now "$u" 2>/dev/null || true; done
  for u in "${units[@]}"; do rm -f "$SYSD/$u"; done
  rm -f "$REFRESH" "$CONF" "$RULES"
  systemctl daemon-reload
  nft delete table inet $TABLE 2>/dev/null || true
  echo "removed: the factory user's traffic is no longer filtered"
  exit 0
fi
if [[ ${1:-} == --status ]]; then
  nft list table inet $TABLE 2>/dev/null || echo "not installed"
  systemctl --no-pager status factory-egress-refresh.service 2>/dev/null | tail -3 || true
  journalctl -k --since -1h --no-pager 2>/dev/null | grep -F 'factory-egress' | tail -20 || true
  exit 0
fi

ipv4() {
  [[ $1 =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
  for o in "${BASH_REMATCH[@]:1}"; do (( 10#$o <= 255 )) || return 1; done
}
mode= allow_ips=()
while [[ $# -gt 0 ]]; do
  case $1 in
    --log-only) mode=log; shift ;;
    --enforce) mode=enforce; shift ;;
    --allow-ip) ipv4 "${2:-}" || die "--allow-ip takes one IPv4 address"; allow_ips+=("$2"); shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[[ -n $mode ]] || die "pick --log-only or --enforce (or --status, --undo)"
uid=$(id -u "$FACTORY_USER" 2>/dev/null) || die "user $FACTORY_USER not found"
for d in ${EXTRA_DOMAINS:-}; do
  [[ $d =~ ^[A-Za-z0-9.-]+$ ]] || die "EXTRA_DOMAINS: not a domain name: $d"
done
# its containers' users: the sub-uid range the factory user owns
uids="$uid"
while IFS=: read -r name start count; do
  if [[ $name == "$FACTORY_USER" || $name == "$uid" ]] && [[ $start =~ ^[0-9]+$ && $count =~ ^[0-9]+$ ]]; then
    uids+=", $start-$((start + count - 1))"
  fi
done </etc/subuid

if [[ $mode == enforce ]]; then
  prefix=factory-egress-refused verdict='meta l4proto tcp counter reject with tcp reset
    counter reject'
else
  prefix=factory-egress-would-refuse verdict='counter accept'
fi
lan=""
if [[ ${#allow_ips[@]} -gt 0 ]]; then
  lan="ip daddr { $(IFS=,; echo "${allow_ips[*]}") } tcp dport 22 accept"
fi

# replaced whole in one transaction, so there is never a moment without rules
cat >"$RULES.new" <<EOF
table inet $TABLE
delete table inet $TABLE
table inet $TABLE {
  set allow4 { type ipv4_addr; flags timeout; timeout 1d; }
  set allow6 { type ipv6_addr; flags timeout; timeout 1d; }
  set dns4 { type ipv4_addr; flags timeout; timeout 1d; }
  set dns6 { type ipv6_addr; flags timeout; timeout 1d; }
  chain output {
    type filter hook output priority filter; policy accept;
    meta skuid { $uids } goto factory
  }
  chain factory {
    oifname "lo" accept
    ct state established,related accept
    ip daddr @dns4 meta l4proto { tcp, udp } th dport 53 accept
    ip6 daddr @dns6 meta l4proto { tcp, udp } th dport 53 accept
    $lan
    ip daddr @allow4 tcp dport { 80, 443 } accept
    ip6 daddr @allow6 tcp dport { 80, 443 } accept
    limit rate 10/minute burst 20 packets log prefix "$prefix " level info
    $verdict
  }
}
EOF
nft -c -f "$RULES.new" || { rm -f "$RULES.new"; die "nft refused the rules; nothing changed"; }
mv "$RULES.new" "$RULES"
chmod 0644 "$RULES"

# The backup bucket (ARCHITECTURE.md §15.2): R2's endpoint is per account, <account>.r2.cloudflarestorage.com
# on 443, so it is read from the factory user's config.toml ([backup] endpoint) rather than listed above.
# Re-run this script after setting or changing it.
r2=""
fhome=$(getent passwd "$FACTORY_USER" | cut -d: -f6)
if [[ -n $fhome && -r $fhome/factory/config.toml ]]; then
  r2=$(awk '/^[[:space:]]*\[/ { s = $0; gsub(/[[:space:]]/, "", s) }
    s == "[backup]" && $1 == "endpoint" { sub(/^[^=]*=[[:space:]]*"/, ""); sub(/".*/, ""); print; exit }' \
    "$fhome/factory/config.toml" | sed -E 's#^https?://##; s#[/:].*##')
fi
case $r2 in
  "") ;;
  *.r2.cloudflarestorage.com) DOMAINS="$DOMAINS $r2"; echo "backup: allowing $r2" ;;
  *) echo "warning: [backup] endpoint $r2 is not an R2 host; add it to EXTRA_DOMAINS if it is meant" >&2 ;;
esac

printf 'DOMAINS="%s %s"\n' "$(echo $DOMAINS)" "${EXTRA_DOMAINS:-}" >"$CONF"
chmod 0644 "$CONF"
cat >"$REFRESH" <<'EOF'
#!/usr/bin/env bash
# Resolve the factory's allowed domains, and the system's DNS servers, into the egress sets in one
# transaction. An address stays allowed for a day after it was last seen.
set -euo pipefail
. /etc/factory-egress.conf
nft list table inet factory_egress >/dev/null || { echo "the factory_egress table is missing" >&2; exit 1; }
batch=$(mktemp)
trap 'rm -f "$batch"' EXIT
n=0
add() { printf 'add element inet factory_egress %s { %s timeout 1d }\n' "$1" "$2" >>"$batch"; n=$((n + 1)); }
for d in $DOMAINS; do
  for a in $(getent ahostsv4 "$d" | awk '{print $1}' | sort -u); do add allow4 "$a"; done
  for a in $(getent ahostsv6 "$d" | awk '$1 ~ /:/ && $1 !~ /^::ffff:/ {print $1}' | sort -u); do add allow6 "$a"; done
done
# the resolvers systemd-resolved (and so the containers' DNS) forwards to
for a in $(awk '$1 == "nameserver" {print $2}' /run/systemd/resolve/resolv.conf 2>/dev/null | sed 's/[%#].*//' | sort -u); do
  case $a in *:*) add dns6 "$a" ;; *) add dns4 "$a" ;; esac
done
[[ $n -gt 0 ]] || { echo "resolved nothing; is DNS working?" >&2; exit 1; }
nft -f "$batch"
echo "allowed $n addresses"
EOF
chmod 0755 "$REFRESH"

cat >"$SYSD/factory-egress-table.service" <<EOF
[Unit]
Description=The factory user's egress table
DefaultDependencies=no
After=local-fs.target
Before=network-pre.target
Wants=network-pre.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f $RULES
ExecStop=/usr/sbin/nft delete table inet $TABLE
[Install]
WantedBy=sysinit.target
EOF
cat >"$SYSD/factory-egress-refresh.service" <<EOF
[Unit]
Description=Refresh the factory's egress allowlist
Wants=network-online.target
After=network-online.target factory-egress-table.service
Requires=factory-egress-table.service
[Service]
Type=oneshot
ExecStart=$REFRESH
EOF
cat >"$SYSD/factory-egress-refresh.timer" <<EOF
[Unit]
Description=Refresh the factory's egress allowlist every 10 minutes
[Timer]
OnBootSec=15s
OnUnitActiveSec=10min
[Install]
WantedBy=timers.target
EOF

systemctl daemon-reload
systemctl enable factory-egress-table.service >/dev/null
systemctl restart factory-egress-table.service
# the timer first: if this first refresh fails (DNS not up yet), it retries on its own
systemctl enable --now factory-egress-refresh.timer >/dev/null
"$REFRESH" || echo "WARNING: the first refresh failed; the timer retries every 10 minutes (sudo bash egress.sh --status)" >&2

echo "installed ($mode) for $FACTORY_USER (uids $uids)"
echo "watch: journalctl -k -f | grep factory-egress    status: sudo bash egress.sh --status"

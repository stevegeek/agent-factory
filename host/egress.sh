#!/usr/bin/env bash
# Narrow the factory user's outbound traffic to the services it uses. Run as root on each host:
#   sudo bash egress.sh --log-only [--allow-ip 192.168.80.11]...   log what would be refused, refuse nothing
#   sudo bash egress.sh --enforce  [--allow-ip ...]                 refuse everything not allowed
#   sudo bash egress.sh --status                                    the rules, the sets, recent refusals
#   sudo bash egress.sh --undo                                      remove all of it
# Optional: FACTORY_USER (default factory), EXTRA_DOMAINS="a.example b.example".
#
# Only the factory user's traffic is filtered (nftables `meta skuid`), which covers its rootless
# containers (their traffic leaves through the user's own network helper). Root, apt and tailscaled are
# untouched. Domains are resolved into address sets by a root timer every 10 minutes; an address stays
# allowed for a day after it was last seen, so CDN round-robins settle in. Loopback, DNS and replies to
# connections already made are always allowed. Start with --log-only for a week, read
# `journalctl -k | grep factory-egress`, add what's missing to EXTRA_DOMAINS, then --enforce.
set -euo pipefail

FACTORY_USER=${FACTORY_USER:-factory}
TABLE=factory_egress
REFRESH=/usr/local/sbin/factory-egress-refresh
CONF=/etc/factory-egress.conf
UNIT=/etc/systemd/system/factory-egress-refresh
DOMAINS="api.anthropic.com claude.ai downloads.claude.ai statsig.anthropic.com console.anthropic.com
github.com api.github.com codeload.github.com objects.githubusercontent.com raw.githubusercontent.com
release-assets.githubusercontent.com
rubygems.org index.rubygems.org registry.npmjs.org
docker.io registry-1.docker.io auth.docker.io production.cloudflare.docker.com
web.push.apple.com api.push.apple.com fcm.googleapis.com updates.push.services.mozilla.com"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "run with sudo"
command -v nft >/dev/null || die "nft not found (apt install nftables)"

if [[ ${1:-} == --undo ]]; then
  systemctl disable --now factory-egress-refresh.timer 2>/dev/null || true
  systemctl disable factory-egress-refresh-table.service 2>/dev/null || true
  rm -f "$UNIT.service" "$UNIT.timer" "$UNIT-table.service" /etc/factory-egress.nft "$REFRESH" "$CONF"
  systemctl daemon-reload
  nft delete table inet $TABLE 2>/dev/null || true
  echo "removed: the factory user's traffic is no longer filtered"
  exit 0
fi
if [[ ${1:-} == --status ]]; then
  nft list table inet $TABLE 2>/dev/null || echo "not installed"
  journalctl -k --since -1h --no-pager 2>/dev/null | grep -F 'factory-egress' | tail -20 || true
  exit 0
fi

mode= allow_ips=()
while [[ $# -gt 0 ]]; do
  case $1 in
    --log-only) mode=log; shift ;;
    --enforce) mode=enforce; shift ;;
    --allow-ip) [[ ${2:-} =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "--allow-ip takes one IPv4 address"
                allow_ips+=("$2"); shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[[ -n $mode ]] || die "pick --log-only or --enforce (or --status, --undo)"
uid=$(id -u "$FACTORY_USER" 2>/dev/null) || die "user $FACTORY_USER not found"
for d in ${EXTRA_DOMAINS:-}; do
  [[ $d =~ ^[A-Za-z0-9.-]+$ ]] || die "EXTRA_DOMAINS: not a domain name: $d"
done

if [[ $mode == enforce ]]; then
  last='log prefix "factory-egress-refused " level info limit rate 10/minute burst 20 packets counter reject'
else
  last='log prefix "factory-egress-would-refuse " level info limit rate 10/minute burst 20 packets counter accept'
fi
lan=""
if [[ ${#allow_ips[@]} -gt 0 ]]; then
  lan="ip daddr { $(IFS=,; echo "${allow_ips[*]}") } tcp dport 22 accept"
fi

# the table is replaced whole in one transaction, so there is never a moment without rules
rules=$(mktemp)
trap 'rm -f "$rules"' EXIT
cat >"$rules" <<EOF
table inet $TABLE
delete table inet $TABLE
table inet $TABLE {
  set allow4 { type ipv4_addr; flags timeout; timeout 1d; }
  set allow6 { type ipv6_addr; flags timeout; timeout 1d; }
  chain output {
    type filter hook output priority filter; policy accept;
    meta skuid != $uid accept
    oifname "lo" accept
    ct state established,related accept
    udp dport 53 accept
    tcp dport 53 accept
    udp dport 123 accept
    $lan
    ip daddr @allow4 tcp dport { 80, 443 } accept
    ip6 daddr @allow6 tcp dport { 80, 443 } accept
    $last
  }
}
EOF
nft -c -f "$rules" || die "nft refused the rules; nothing changed"

printf 'DOMAINS="%s %s"\n' "$(echo $DOMAINS)" "${EXTRA_DOMAINS:-}" >"$CONF"
chmod 0644 "$CONF"
cat >"$REFRESH" <<'EOF'
#!/usr/bin/env bash
# Resolve the factory's allowed domains into the egress sets (an address stays for a day after it was seen).
set -u
. /etc/factory-egress.conf
for d in $DOMAINS; do
  getent ahostsv4 "$d" | awk '{print $1}' | sort -u | while read -r a; do
    nft add element inet factory_egress allow4 "{ $a timeout 1d }" 2>/dev/null
  done
  getent ahostsv6 "$d" | awk '$1 ~ /:/ && $1 !~ /^::ffff:/ {print $1}' | sort -u | while read -r a; do
    nft add element inet factory_egress allow6 "{ $a timeout 1d }" 2>/dev/null
  done
done
exit 0
EOF
chmod 0755 "$REFRESH"
cat >"$UNIT.service" <<EOF
[Unit]
Description=Refresh the factory's egress allowlist
After=network-online.target
[Service]
Type=oneshot
ExecStart=$REFRESH
EOF
cat >"$UNIT.timer" <<EOF
[Unit]
Description=Refresh the factory's egress allowlist every 10 minutes
[Timer]
OnBootSec=1min
OnUnitActiveSec=10min
[Install]
WantedBy=timers.target
EOF

nft -f "$rules"
"$REFRESH"
systemctl daemon-reload
systemctl enable --now factory-egress-refresh.timer >/dev/null

# keep the table across reboots: the timer re-creates elements, but the table must exist first
cat >"$UNIT-table.service" <<EOF
[Unit]
Description=The factory's egress table
Before=network-pre.target factory-egress-refresh.service
Wants=network-pre.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/factory-egress.nft
[Install]
WantedBy=multi-user.target
EOF
cp "$rules" /etc/factory-egress.nft
chmod 0644 /etc/factory-egress.nft
systemctl daemon-reload
systemctl enable factory-egress-refresh-table.service >/dev/null

n4=$(nft list set inet $TABLE allow4 | grep -c timeout || true)
echo "installed ($mode) for $FACTORY_USER (uid $uid): $n4 IPv4 lines in the allowlist"
echo "watch: journalctl -k -f | grep factory-egress    status: sudo bash egress.sh --status"

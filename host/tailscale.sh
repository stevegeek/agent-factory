#!/usr/bin/env bash
# Tailscale on a factory host, so Foreman (and SSH, if wanted) is reachable over the tailnet.
# Run as root on the host:
#   sudo bash tailscale.sh            # install from Tailscale's signed apt repo (safe to re-run)
#   sudo bash tailscale.sh --undo     # log out, remove the package, the repo and its key
# Optional: FACTORY_NAME (names the files written here).
#
# Does:
#   1. adds pkgs.tailscale.com's apt repo for this Ubuntu release, with its signing key checked
#      against the published fingerprint, and installs tailscale
#   2. lets unattended-upgrades update tailscale with the rest of the OS
#   3. enables tailscaled. It does not log in: that needs a browser, so the next steps are printed.
# It does not touch sshd or DNS. tailscaled adds its own ts-* firewall chains for tailnet
# traffic; they leave LAN SSH alone.
set -euo pipefail

NAME=${FACTORY_NAME:-agent-factory}
KEYRING=/usr/share/keyrings/tailscale-archive-keyring.gpg
LIST=/etc/apt/sources.list.d/tailscale.list
APT_CONF=/etc/apt/apt.conf.d/52$NAME-tailscale
# Tailscale Inc. (Package repository signing key) <info@tailscale.com>
FINGERPRINT=2596A99EAAB33821893C0A79458CA832957F5868

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "run with sudo"

if [[ ${1:-} == --undo ]]; then
  if dpkg -s tailscale >/dev/null 2>&1; then
    tailscale serve reset 2>/dev/null || true
    tailscale logout 2>/dev/null || true
    # purge also deletes /var/lib/tailscale; wait out unattended-upgrades rather than fail on its lock
    apt-get -o DPkg::Lock::Timeout=300 purge -y tailscale
    apt-get -o DPkg::Lock::Timeout=300 autoremove -y
  fi
  rm -f "$LIST" "$KEYRING" "$APT_CONF"
  apt-get update -qq
  echo "removed tailscale, its repo, key and state."
  echo "The machine still appears in the tailnet's admin console until you delete it there."
  exit 0
fi
[[ -z ${1:-} ]] || die "usage: tailscale.sh [--undo]"

# a subshell: os-release also sets NAME, which would replace ours
ID=$(. /etc/os-release && printf %s "${ID:-}")
VERSION_CODENAME=$(. /etc/os-release && printf %s "${VERSION_CODENAME:-}")
[[ ${ID:-} == ubuntu && -n ${VERSION_CODENAME:-} ]] || die "expected Ubuntu, found ${ID:-unknown}"
command -v curl >/dev/null || die "curl is not installed"
command -v gpg >/dev/null || die "gpg is not installed"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://pkgs.tailscale.com/stable/ubuntu/$VERSION_CODENAME.noarmor.gpg" -o "$tmp/key.gpg"
# exactly one primary key, and it is Tailscale's: a file carrying an extra key must not be trusted
found=$(gpg --show-keys --with-colons "$tmp/key.gpg" |
  awk -F: '$1 == "pub" {p = 1; n++; next} $1 == "fpr" && p {print $10; p = 0} END {if (n != 1) exit 1}') ||
  die "Tailscale's key file does not hold exactly one key"
[[ $found == "$FINGERPRINT" ]] || die "Tailscale's repo key does not match $FINGERPRINT (got: ${found:-none})"
install -m 644 "$tmp/key.gpg" "$KEYRING"

echo "deb [signed-by=$KEYRING] https://pkgs.tailscale.com/stable/ubuntu $VERSION_CODENAME main" > "$LIST"
chmod 644 "$LIST"
apt-get update -qq
apt-get install -y tailscale

# the repo's Release file says Origin: Tailscale
cat > "$APT_CONF" <<EOF
// agent factory: keep tailscale updated with the OS
Unattended-Upgrade::Origins-Pattern:: "origin=Tailscale,codename=$VERSION_CODENAME";
EOF
chmod 644 "$APT_CONF"

systemctl enable --now tailscaled
tailscale version | head -1
cat <<'EOF'

Installed. First, in the tailnet's policy file: a tag you own ("tagOwners": {"tag:factory": [...]})
and a rule letting your devices reach it (src your devices, dst tag:factory:443), with no rule whose
src is tag:factory, so this host can answer but never start a connection to your other machines.
Then, as an admin on this host:
  sudo tailscale up --advertise-tags=tag:factory --accept-dns=false
                                           # prints a login URL; open it and approve the machine
                                           # (tagged: not you; keeps this server's own DNS)
  sudo tailscale serve --bg 8790           # Foreman over HTTPS on the tailnet (it listens on 127.0.0.1:8790)
  tailscale serve status                   # shows the https://<host>.<tailnet>.ts.net URL
Foreman can then trust the tailnet login: --trust-tailscale --tailscale-user <your login> on its unit.
EOF

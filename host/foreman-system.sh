#!/usr/bin/env bash
# foreman-system.sh: run Foreman as a system unit under the factory user, so systemd can apply its
# whole sandbox. A user manager on this host cannot (no unprivileged mount namespaces for it), so as a
# user unit Foreman could read the GitHub App key and reach the user manager, through which any
# process runs any command outside every sandbox.
#
#   sudo bash foreman-system.sh [--user factory]     install, start, check
#   sudo bash foreman-system.sh --undo [--user ...]  back to the user unit (then redeploy)
#
# Installs /etc/systemd/system/foreman.service (User=factory, sandboxed, in its own user namespace so
# it cannot read other factory processes through /proc), and foreman-restart.path + .service, which
# restart Foreman when deploy.sh replaces bin/foreman or rewrites foreman.env (the arguments rendered
# from config.toml), or the token store changes. Disables the factory user's own foreman unit;
# deploy.sh sees the system unit and stops managing that one. Then checks from inside Foreman's
# namespaces that the App key, the user manager and other factory processes are out of its reach.
set -euo pipefail

user=factory undo=0
while [ $# -gt 0 ]; do
  case "$1" in
    --user) user=${2:?--user needs a name}; shift ;;
    --undo) undo=1 ;;
    -h|--help) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "foreman-system.sh: unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
die() { echo "foreman-system.sh: $*" >&2; exit 1; }
say() { echo "foreman-system.sh: $*" >&2; }
[ "$(id -u)" = 0 ] || die "run it as root (sudo)"
case "$user" in ''|*[!a-z0-9_-]*) die "$user is not a plain user name" ;; esac
uid=$(id -u "$user" 2>/dev/null) || die "no user $user"
[ "$uid" != 0 ] || die "refusing to run Foreman as root"
gid=$(id -g "$user")
group=$(id -gn "$user")
home=$(getent passwd "$user" | cut -d: -f6)
case "$home" in /*) ;; *) die "$user has no absolute home" ;; esac
case "$home" in *[!A-Za-z0-9/._-]*) die "$home has characters a unit file would need escaped" ;; esac
F=$home/factory
c=$home/.config/factory
S=/etc/systemd/system

# as_user CMD...: the factory user's own systemd (lingering), for its user unit
as_user() { runuser -u "$user" -- env XDG_RUNTIME_DIR="/run/user/$uid" "$@"; }

if [ $undo = 1 ]; then
  systemctl disable --now foreman-restart.path foreman.service 2>/dev/null || true
  rm -f "$S/foreman.service" "$S/foreman-restart.path" "$S/foreman-restart.service"
  systemctl daemon-reload
  if [ -f "$home/.config/systemd/user/foreman.service" ]; then
    as_user systemctl --user enable --now foreman || say "warning: could not start the user unit"
  fi
  say "undone: Foreman is the factory user's unit again (not sandboxed on this host);"
  say "run deploy.sh once so the user unit is current"
  exit 0
fi

[ -x "$F/bin/foreman" ] || die "$F/bin/foreman is missing: deploy first"
[ -f "$F/foreman.env" ] || die "$F/foreman.env is missing: deploy with a deploy.sh that writes it first"
grep -q '^FOREMAN_ARGS=' "$F/foreman.env" || die "$F/foreman.env has no FOREMAN_ARGS line"
for d in "$F/board" "$F/logs" "$F/foreman-state"; do [ -d "$d" ] || die "$d is missing"; done
[ -f "$c/api-tokens.json" ] || say "note: no $c/api-tokens.json yet: the API answers no token until one exists and Foreman restarts"

trap 'say "failed partway: Foreman may be down. Undo with: sudo bash $0 --undo"' ERR

cat >"$S/foreman.service.new" <<EOF
# Installed by host/foreman-system.sh: Foreman as a system unit under $user, fully sandboxed.
# Undo: sudo bash foreman-system.sh --undo
[Unit]
Description=Foreman: the factory's desk for its maintainer
After=network.target user@$uid.service

[Service]
User=$user
Group=$group
Environment=XDG_RUNTIME_DIR=/run/user/$uid
# foreman.env is read by the shell as $user, never by systemd as root
ExecStart=/bin/sh -fc '. $F/foreman.env; exec $F/bin/foreman \$\$FOREMAN_ARGS'
Restart=on-failure
RestartSec=5
MemoryMax=128M
UMask=0077
# its own user namespace: other $user processes (the tick, the user manager, agents) are not
# readable through /proc, so their mounts, files and environment stay out of reach
PrivateUsers=yes
ProtectProc=invisible
ProcSubset=pid
# what it may write: the board and its own state; everything else is read-only, logs/ included
# (it logs to the journal and foreman-state/foreman.log), so it can plant nothing there for the tick to follow
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=$F/board $F/foreman-state
# the credentials directory is an empty read-only tmpfs, with only the token store bound back
TemporaryFileSystem=$c:ro
BindReadOnlyPaths=-$c/api-tokens.json
InaccessiblePaths=-$home/.claude -$home/.config/gh
# no user manager (through which anything runs outside this sandbox), no podman, no system bus;
# Live's local screens come from the relay (factory-screens.path, a user unit), which it only reads
InaccessiblePaths=/run/user
InaccessiblePaths=-/run/dbus
PrivateTmp=yes
PrivateDevices=yes
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictNamespaces=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF
cat >"$S/foreman-restart.path.new" <<EOF
# Installed by host/foreman-system.sh: restart Foreman when a deploy replaces it or its arguments,
# or the token store changes (it is bound into Foreman's view by inode).
[Path]
PathChanged=$F/bin/foreman
PathChanged=$F/foreman.env
PathChanged=$c/api-tokens.json
Unit=foreman-restart.service

[Install]
WantedBy=multi-user.target
EOF
cat >"$S/foreman-restart.service.new" <<'EOF'
# Installed by host/foreman-system.sh.
[Service]
Type=oneshot
ExecCondition=/bin/systemctl -q is-enabled foreman.service
ExecStart=/bin/systemctl restart foreman.service
EOF
for f in foreman.service foreman-restart.path foreman-restart.service; do
  chmod 644 "$S/$f.new"
  mv "$S/$f.new" "$S/$f"
done
systemd-analyze verify "$S/foreman.service" "$S/foreman-restart.path" "$S/foreman-restart.service" ||
  say "warning: systemd-analyze reported the above (it can be noisy about other units)"

# the user unit stops first: both would listen on the same address
as_user systemctl --user disable --now foreman 2>/dev/null || true
if as_user systemctl --user -q is-active foreman 2>/dev/null; then die "the user unit foreman is still running"; fi
systemctl daemon-reload
systemctl enable foreman.service foreman-restart.path
systemctl restart foreman.service
systemctl start foreman-restart.path

for i in $(seq 1 20); do
  [ "$(systemctl show -P ActiveState foreman)" = active ] && break
  sleep 1
done
[ "$(systemctl show -P ActiveState foreman)" = active ] || die "foreman did not start: see journalctl -u foreman"
sleep 2 # let the shell exec Foreman before reading its pid
fp=$(systemctl show -P MainPID foreman)
[ "$fp" != 0 ] || die "foreman has no main process"
[ "$(readlink "/proc/$fp/exe")" = "$F/bin/foreman" ] || die "foreman's main process is not $F/bin/foreman yet: rerun the script"

# check from inside Foreman's namespaces, as Foreman sees things
inside() { nsenter -t "$fp" -a -S "$uid" -G "$gid" -- "$@"; }
[ "$(inside sh -c 'id -u; readlink /proc/self/ns/user')" = "$uid
$(readlink "/proc/$fp/ns/user")" ] || die "cannot enter Foreman's namespaces as $user: checks not run (Foreman is running; --undo to revert)"
um=$(systemctl show -P MainPID "user@$uid.service")
leaks=""
inside test -r "$c/github-app.pem" 2>/dev/null && leaks="$leaks github-app.pem"
inside test -r "$c/claude-oauth-token" 2>/dev/null && leaks="$leaks claude-oauth-token"
inside test -e "/run/user/$uid/bus" 2>/dev/null && leaks="$leaks /run/user/$uid/bus"
if [ -n "$um" ] && [ "$um" != 0 ]; then
  inside test -r "/proc/$um/environ" 2>/dev/null && leaks="$leaks /proc/<user manager>/environ"
  inside test -r "/proc/$um/root$c/github-app.pem" 2>/dev/null && leaks="$leaks /proc/<user manager>/root"
fi
# a real write, not test -w: permission bits say yes even where the mount is read-only
if inside sh -c ": >'$F/logs/.foreman-probe'" 2>/dev/null; then
  leaks="$leaks write:logs/"
  rm -f "$F/logs/.foreman-probe"
fi
[ -z "$leaks" ] || die "Foreman can still reach:$leaks (it is running; undo with --undo)"
inside test -r "$F/config.toml" || die "Foreman cannot read $F/config.toml"
inside sh -c ": >'$F/board/.foreman-probe'" || die "Foreman cannot write $F/board"
rm -f "$F/board/.foreman-probe"

listen=$(sed -n 's/^FOREMAN_ARGS=//p' "$F/foreman.env" | tr -d '"' | awk '{ for (i = 1; i < NF; i++) if ($i == "--listen") { print $(i + 1); exit } }')
case "$listen" in
  0.0.0.0:*) listen=127.0.0.1:${listen#0.0.0.0:} ;;
  "[::]:"*) listen="[::1]:${listen#\[::\]:}" ;;
esac
[ -n "$listen" ] || die "no --listen in foreman.env: cannot check /healthz"
body=""
for i in $(seq 1 15); do
  body=$(curl -fsS --max-time 3 "http://$listen/healthz" 2>/dev/null) && break
  sleep 1
done
case "$body" in
  *exposed:*) die "Foreman's /healthz: $(printf '%s' "$body" | tr '\n' ' ')" ;;
  ok*) say "done: Foreman runs as a sandboxed system unit; the App key, the user manager and other factory processes are out of its reach" ;;
  *) die "Foreman did not answer /healthz on $listen: see journalctl -u foreman (undo: --undo)" ;;
esac

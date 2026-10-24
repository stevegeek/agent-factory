#!/usr/bin/env bash
# deploy.sh: build the factory's Go binaries, render a config directory, and install both on a
# host as the factory user over SSH.
#
#   deploy.sh --config DIR --host SSH_HOST [--build host|local|helper] [--verify] [--helper SSH_HOST]...
#             [--install-go] [--commit REV] [--core GITDIR] [--prebuilt DIR] [--allow-older] [--dry-run]
#
#   --build host     build on the host, with its pinned toolchain (the default)
#   --build local    build here, with the same pinned toolchain, and copy the binaries across
#   --build helper   the host has the builder helper ([build] host) build it (factory build) and
#                    installs that candidate (README "Builds on a helper")
#   --verify         build in both places; install only if the two BUILD files are identical
#   --helper HOST    also install factory-remote and its files on a helper host (repeatable),
#                    relayed through this machine: the dispatcher's key cannot copy there
#   --install-go     put the pinned Go toolchain on the host first: downloaded here and checked
#                    against build/go.sha256 (FACTORY_GO_TARBALL names a tarball already here)
#   --commit REV     the commit of core to deploy (default HEAD; uncommitted changes never ship)
#   --core GITDIR    take the commit from GITDIR (a bare one will do) instead of this script's
#                    checkout; --commit is then required
#   --prebuilt DIR   install the binaries and BUILD file in DIR instead of building
#   --allow-older    install even a core or config that lacks what the host runs (a rollback)
#   --dry-run        deploy into a new temporary directory on this machine and list what it holds
#
# A HOST of the form local:DIR deploys into DIR on this machine as if DIR were the factory user's
# home: no ssh, and no systemctl or systemd-run (what would run there is printed).
# A HOST of self is the master deploying itself (factory deploy run, which sets
# FACTORY_DEPLOY_SELF=1 and FACTORY_DEPLOY_DIR, the staging directory): no ssh, real systemd.
#
# The host keeps its board, logs, memory, token store, images.json, backup key and secrets.
# config.toml.prev keeps the previous config, and bin/<binary>.prev the previous Go binary.
# deploys/config-commit records the config commit when --config is in a git checkout.
# A deploy refuses a core commit that does not contain the core the host runs (bin/BUILD), and
# a config checkout whose HEAD does not contain deploys/config-commit, or a --config that is no
# git checkout while the host records one: a lagging core/ submodule or an unpulled config would
# roll the host back. --host self checks the core only: its runner deploys config main.
set -euo pipefail
umask 022

core=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
die() { printf 'deploy.sh: %s\n' "$*" >&2; exit 1; }
say() { printf 'deploy.sh: %s\n' "$*" >&2; }
usage() { sed -n '4,5p' "$0" | sed 's/^# \{0,1\}//' >&2; [ -z "${1:-}" ] || printf 'deploy.sh: %s\n' "$1" >&2; exit 2; }

config="" host="" build=host verify=0 install_go=0 rev=HEAD prebuilt="" dry=0 core_dir="" rev_set=0 allow_older=0
helpers=()
while [ $# -gt 0 ]; do
  case "$1" in
    --config) [ $# -ge 2 ] || usage "--config needs a directory"; config=$2; shift ;;
    --host) [ $# -ge 2 ] || usage "--host needs a host"; host=$2; shift ;;
    --helper) [ $# -ge 2 ] || usage "--helper needs a host"; helpers+=("$2"); shift ;;
    --build)
      [ $# -ge 2 ] || usage "--build needs host, local or helper"
      case "$2" in host|local|helper) build=$2 ;; *) usage "--build is host, local or helper, not $2" ;; esac
      shift ;;
    --verify) verify=1 ;;
    --install-go) install_go=1 ;;
    --commit) [ $# -ge 2 ] || usage "--commit needs a revision"; rev=$2; rev_set=1; shift ;;
    --core) [ $# -ge 2 ] || usage "--core needs a git directory"; core_dir=$2; shift ;;
    --prebuilt) [ $# -ge 2 ] || usage "--prebuilt needs a directory"; prebuilt=$2; shift ;;
    --dry-run) dry=1 ;;
    --allow-older) allow_older=1 ;;
    -h|--help) sed -n '2,34p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) usage "unknown argument: $1" ;;
  esac
  shift
done
[ -n "$config" ] || usage "--config is required"
[ $dry = 1 ] || [ -n "$host" ] || usage "--host is required (or --dry-run)"
[ $dry = 0 ] || [ -z "$host" ] || usage "--dry-run deploys into a temporary directory: leave out --host"
[ -z "$prebuilt" ] || [ $verify = 0 ] || usage "--verify builds: it does not go with --prebuilt"
[ "$build" != helper ] || [ $verify = 0 ] || usage "--verify compares two builds here and on the host: not with --build helper"
[ "$build" != helper ] || [ -z "$prebuilt" ] || usage "--prebuilt installs DIR: not with --build helper"
[ "$build" != helper ] || [ $dry = 0 ] || usage "--dry-run has no host to build from: not with --build helper"
[ -z "$core_dir" ] || [ $rev_set = 1 ] || usage "--core needs --commit: a git directory has no HEAD to deploy"
if [ "$host" = self ]; then
  [ "${FACTORY_DEPLOY_SELF:-}" = 1 ] || die "--host self is for factory deploy run on the master"
  [ -n "${FACTORY_DEPLOY_DIR:-}" ] || die "--host self needs FACTORY_DEPLOY_DIR, its staging directory"
  [ ${#helpers[@]} = 0 ] || die "--helper with --host self: the master sends its helpers the install verb itself"
fi
[ -f "$config/config.toml" ] || die "$config has no config.toml"
config=$(cd "$config" && pwd -P)
# the config commit this installs, when --config is in a git checkout: the host keeps it in
# deploys/config-commit (the stale-checkout guard reads it). A self-deploy's runner writes it
# itself, after its check.
config_commit=""
if [ "$host" != self ] && git -C "$config" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  config_commit=$(git -C "$config" rev-parse --verify -q HEAD) || config_commit=""
  if [ -n "$config_commit" ] && [ -n "$(git -C "$config" status --porcelain --untracked-files=no -- . 2>/dev/null)" ]; then
    say "warning: the config checkout $config has uncommitted changes: they ship, and deploys/config-commit records HEAD"
  fi
fi
if [ -n "$core_dir" ]; then
  [ -d "$core_dir" ] || die "no such directory: $core_dir"
  core=$(cd "$core_dir" && pwd -P)
fi

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{ print $1 }'; }

# check_build DIR: DIR holds the three binaries, each matching its line in DIR/BUILD.
check_build() {
  local d=$1 b n
  [ -f "$d/BUILD" ] || die "$d has no BUILD file"
  for b in factory foreman factory-remote; do
    [ -f "$d/$b" ] || die "$d has no $b"
    n=$(awk -v b="$b" '$2 == b { print $1 }' "$d/BUILD")
    [ -n "$n" ] || die "$d/BUILD names no $b"
    [ "$(sha "$d/$b")" = "$n" ] || die "$d/$b: $b does not match its sha256 in BUILD"
  done
}

# ---------------------------------------------------------------- what ships
commit=$(git -C "$core" rev-parse --verify --short=12 "$rev^{commit}" 2>/dev/null) || die "$rev is not a commit of $core"
if [ "$rev" = HEAD ] && [ -n "$(git -C "$core" status --porcelain --untracked-files=no 2>/dev/null)" ]; then
  say "warning: $core has uncommitted changes; they do not ship (deploying $commit)"
fi
pin=$(git -C "$core" show "$commit:go.mod" | awk '$1 == "toolchain" { print $2 }')
[ -n "$pin" ] || die "go.mod at $commit has no toolchain line"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git -C "$core" archive --format=tar "$commit" >"$tmp/src.tar"
parts=(config.toml)
[ -e "$config/rules.md" ] && parts+=(rules.md)
[ -d "$config/context" ] && parts+=(context)
tar -C "$config" -cf "$tmp/config.tar" "${parts[@]}"

if [ -n "$prebuilt" ]; then
  prebuilt=$(cd "$prebuilt" && pwd -P)
  check_build "$prebuilt"
fi

tarball=""
if [ $install_go = 1 ]; then
  name=$pin.linux-amd64.tar.gz
  tarball=${FACTORY_GO_TARBALL:-}
  if [ -z "$tarball" ]; then
    dl=${XDG_CACHE_HOME:-$HOME/.cache}/factory-go/dl
    mkdir -p "$dl"
    tarball=$dl/$name
    if [ ! -f "$tarball" ]; then
      say "downloading $name"
      curl -fsSL --proto '=https' --max-time 900 -o "$tarball.part" "https://go.dev/dl/$name" || die "could not download $name"
      mv "$tarball.part" "$tarball"
    fi
  fi
  [ -f "$tarball" ] || die "no such tarball: $tarball"
  [ "$(basename "$tarball")" = "$name" ] || die "$tarball is not $name"
  sum=$(sha "$tarball")
  git -C "$core" show "$commit:build/go.sha256" | awk -v s="$sum" -v n="$name" '$1 == s && $2 == n { f = 1 } END { exit !f }' ||
    die "$name has sha256 $sum, which is not pinned in build/go.sha256: refusing it"
fi

if [ $dry = 1 ]; then
  for h in "${helpers[@]}"; do
    case "$h" in local:*) ;; *) usage "--dry-run touches no host: a --helper must be local:DIR" ;; esac
  done
  host=local:$(mktemp -d)
fi

# ---------------------------------------------------------------- the host side
# The script that runs on each host, as the factory user, one verb per call.
cat >"$tmp/remote.sh" <<'REMOTE'
set -euo pipefail
umask 022
F=$HOME/factory
D=$F/.deploy
# factory deploy run stages in a directory of its own, never .deploy, which the desktop's
# send_script rewrites in place
if [ "${FACTORY_DEPLOY_SELF:-}" = 1 ] && [ -n "${FACTORY_DEPLOY_DIR:-}" ]; then D=$FACTORY_DEPLOY_DIR; fi
say() { printf 'deploy.sh: %s: %s\n' "${FACTORY_DEPLOY_NAME:-host}" "$*" >&2; }
die() { say "$*"; exit 1; }
local_mode() { [ "${FACTORY_DEPLOY_LOCAL:-}" = 1 ]; }
# sysd runs a systemd command; in local mode it only says what would run.
sysd() { if local_mode; then printf 'would run: %s\n' "$*" >&2; return 0; fi; "$@"; }
# sysq asks systemd a question; in local mode the answer is no.
sysq() { if local_mode; then return 1; fi; "$@" 2>/dev/null; }
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{ print $1 }'; }
is_elf() { [ -f "$1" ] && [ ! -L "$1" ] && [ "$(head -c 4 "$1" | od -An -tx1 | tr -d ' \n')" = 7f454c46 ]; }
word() { case "$1" in ''|*[!A-Za-z0-9._-]*) die "$1 is not one word" ;; esac; }

check_build() {
  local d=$1 b n
  [ -f "$d/BUILD" ] || die "$d has no BUILD file"
  for b in "${@:2}"; do
    [ -f "$d/$b" ] || die "$d has no $b"
    n=$(awk -v b="$b" '$2 == b { print $1 }' "$d/BUILD")
    [ -n "$n" ] || die "$d/BUILD names no $b"
    [ "$(sha "$d/$b")" = "$n" ] || die "$d/$b: $b does not match its sha256 in BUILD"
  done
}

# install_bin NEW DEST: an atomic rename over DEST, so a running binary keeps its old inode.
# Only a previous Go binary is kept, as DEST.prev (a hard link to the old inode).
install_bin() {
  chmod 755 "$1"
  # .prev keeps the last different binary: a reused build must not overwrite it with itself
  if is_elf "$2" && ! cmp -s "$1" "$2"; then ln -f "$2" "$2.prev"; fi
  mv -f "$1" "$2"
}

# install_file NEW DEST [MODE]: copy beside, then rename over.
install_file() {
  cp "$1" "$2.deploy.tmp"
  [ -z "${3:-}" ] || chmod "$3" "$2.deploy.tmp"
  mv -f "$2.deploy.tmp" "$2"
}

# take_tick_lock: hold .tick.lock (fd 9) so no tick runs while files change; a timer tick that
# fires meanwhile only leaves .tick.again. A tick runs at most 45 minutes.
take_tick_lock() {
  exec 9>>"$F/.tick.lock"
  if command -v flock >/dev/null 2>&1; then
    say "waiting for the tick lock"
    flock -w 3000 9 || die "a tick held .tick.lock for 50 minutes: nothing installed"
  else
    say "warning: no flock here: installing without the tick lock"
  fi
}

# The Foreman unit's sandbox: the Key=Value lines between its markers.
sandbox_lines() { awk '/^# sandbox:begin/ { on = 1; next } /^# sandbox:end/ { on = 0 } on && NF && $1 !~ /^#/' "$1"; }
sandbox_props() {
  local l rt=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
  while IFS= read -r l; do
    l=${l//%h/$HOME}
    printf -- '-p\n%s\n' "${l//%t/$rt}"
  done < <(sandbox_lines "$1")
}
# in_sandbox UNIT CMD...: run CMD under the unit's sandbox through the user manager.
in_sandbox() {
  local unit=$1 props=()
  shift
  mapfile -t props < <(sandbox_props "$unit")
  timeout 120 systemd-run --user --wait --pipe --quiet --collect "${props[@]}" "$@" </dev/null
}

# Foreman's own files, moved once from beside the old secret into foreman-state/.
FOREMAN_STATE=(foreman-secret foreman-secret.since foreman-users.json foreman-vapid.pem foreman-push.json foreman-seen.json)

v_prepare() {
  local c=$1 s
  word "$c"
  # factory deploy run holds deploys/.lock for its whole run
  if [ "${FACTORY_DEPLOY_SELF:-}" != 1 ] && [ -f "$F/deploys/.lock" ] && command -v flock >/dev/null 2>&1 &&
    ! flock -n "$F/deploys/.lock" true; then
    # the runner refuses a young .deploy, so a refused deploy leaves none behind
    rm -rf "$D"
    die "a self-deploy is running here (factory deploy status): nothing changed"
  fi
  mkdir -p "$F/bin" "$F/systemd" "$F/images" "$F/board/tickets" "$F/logs" "$F/gate" "$F/src" \
    "$HOME/.local/bin" "$HOME/.scion/templates"
  mkdir -p -m 700 "$F/foreman-state"
  chmod 700 "$F/src" "$F/foreman-state"
  s=$F/src/$c
  rm -rf "$s.new"
  mkdir -m 700 "$s.new"
  tar -C "$s.new" -xf -
  printf '%s\n' "$c" >"$s.new/COMMIT"
  chmod 700 "$s.new"
  rm -rf "$s"
  mv "$s.new" "$s"
}

v_install_go() {
  local c=$1 name=$2 src=$F/src/$1
  word "$c"; word "$name"
  cat >"$D/$name"
  if "$src/build.sh" --check-toolchain >/dev/null 2>&1; then
    say "the pinned toolchain is already installed"
  else
    "$src/build.sh" --install-toolchain "$D/$name"
  fi
  rm -f "$D/$name"
}

v_build() {
  local c=$1 src=$F/src/$1 pin args
  word "$c"
  rm -rf "$F/bin/.new"
  args=(--out "$F/bin/.new")
  local_mode && args+=(--no-box)
  "$src/build.sh" "${args[@]}" >&2
  check_build "$F/bin/.new" factory foreman factory-remote
  # a new pin is a new directory: the old toolchains go once a build with the new one worked
  if [ -z "${FACTORY_GOROOT:-}" ] && [ -d "$HOME/.local/share/factory/go" ]; then
    pin=$(awk '$1 == "toolchain" { print $2 }' "$src/go.mod")
    for d in "$HOME/.local/share/factory/go"/go*; do
      [ -d "$d" ] && [ "$(basename "$d")" != "$pin" ] && { say "removing the old toolchain $d"; rm -rf "$d"; }
    done
  fi
  return 0
}

v_receive() {
  rm -rf "$F/bin/.new"
  mkdir -p "$F/bin/.new"
  tar -C "$F/bin/.new" --no-same-owner -xf -
  check_build "$F/bin/.new" factory foreman factory-remote
  chmod 755 "$F/bin/.new/factory" "$F/bin/.new/foreman" "$F/bin/.new/factory-remote"
}

v_build_id() { cat "$F/bin/.new/BUILD"; }

# candidate COMMIT: COMMIT's build from the builder helper as bin/.new, checked against its BUILD.
# With no candidate yet, the installed factory builds one first (factory build: the source is the
# tree prepare just put in src/COMMIT).
v_candidate() {
  word "$1"
  local c=$F/builds/$1
  if [ ! -f "$c/BUILD" ]; then
    # the installed factory and config build it: the first deploy with [build] goes --build host or local
    "$F/bin/factory" build "$1" >&2 || die "factory build $1 failed: nothing installed (a first deploy of [build] or of factory build itself needs --build host or local)"
  fi
  [ -f "$c/BUILD" ] || die "no candidate for $1 in $c"
  rm -rf "$F/bin/.new"
  mkdir -p "$F/bin/.new"
  cp "$c/factory" "$c/foreman" "$c/factory-remote" "$c/BUILD" "$F/bin/.new/"
  chmod 755 "$F/bin/.new/factory" "$F/bin/.new/foreman" "$F/bin/.new/factory-remote"
  check_build "$F/bin/.new" factory foreman factory-remote
  say "$1 was built on $(cat "$c/HOST" 2>/dev/null || echo 'a helper'): its candidate is staged"
}

# reuse COMMIT: when the installed binaries are COMMIT's build (a deploy that changes only the
# config), copy them to bin/.new, checked against their BUILD, instead of building again.
# Fails, and the caller builds, when they are another commit's or there are none.
# running: what this host runs, "CORE12 CONFIG" with - for one it does not know (a fresh host):
# the commit line of bin/BUILD, as reuse reads it, and deploys/config-commit.
v_running() {
  local b=$F/bin/BUILD c="" cc=""
  [ ! -f "$b" ] || c=$(awk '$1 == "commit" { print $2 }' "$b")
  [ ! -f "$F/deploys/config-commit" ] || cc=$(head -n 1 "$F/deploys/config-commit")
  printf '%s %s\n' "${c:--}" "${cc:--}"
}

v_reuse() {
  local b=$F/bin/BUILD
  word "$1"
  [ -f "$b" ] && [ "$(awk '$1 == "commit" { print $2 }' "$b")" = "$1" ] || return 1
  rm -rf "$F/bin/.new"
  mkdir -p "$F/bin/.new"
  cp -p "$F/bin/factory" "$F/bin/foreman" "$F/bin/factory-remote" "$b" "$F/bin/.new/" || return 1
  check_build "$F/bin/.new" factory foreman factory-remote
  say "the installed binaries are $1's build: reused, nothing built"
}

# stage: render the config with the new factory and smoke-test the new binaries; nothing
# outside .deploy and bin/.new changes.
v_stage() {
  local n=$F/bin/.new
  check_build "$n" factory foreman factory-remote
  rm -rf "$D/config" "$D/out" "$D/root"
  mkdir -p "$D/config" "$D/root"
  tar -C "$D/config" --no-same-owner --no-same-permissions -xf -
  "$n/factory" render "$D/config" "$D/out" >/dev/null || die "factory render refused the config: nothing installed"
  [ -f "$D/out/systemd/foreman.service" ] && [ -f "$D/out/config.toml" ] || die "factory render left out files: nothing installed"
  cp "$D/out/config.toml" "$D/root/config.toml"
  say "new build: $("$n/factory" version)"
  FACTORY_ROOT=$D/root "$n/factory" check >&2 || die "factory check found problems in the new config: nothing installed"
  # the new binary reads the live root cleanly (check only reads): every ticket file
  # decodes, and every ticket in an ask column has its stored ask
  if [ -f "$F/config.toml" ]; then
    FACTORY_ROOT=$F "$n/factory" check --board --asks >&2 ||
      die "factory check --board --asks found problems on the live root: nothing installed"
  fi
  "$n/factory" model >/dev/null || die "the new factory model failed: nothing installed"
  "$n/foreman" --help >/dev/null 2>&1 || die "the new foreman --help failed: nothing installed"
  "$n/factory-remote" version >/dev/null || die "the new factory-remote version failed: nothing installed"
}

# move_foreman_state: once, from ~/.config/factory to foreman-state/, with Foreman stopped.
move_foreman_state() {
  local old=$HOME/.config/factory new=$F/foreman-state f stopped=0
  for f in "${FOREMAN_STATE[@]}"; do
    if [ -e "$old/$f" ] && [ ! -e "$new/$f" ]; then
      if [ $stopped = 0 ]; then
        stopped=1
        if sysq systemctl --user -q is-active foreman; then sysd systemctl --user stop foreman; foreman_was_active=1; fi
      fi
      mv "$old/$f" "$new/$f"
      say "moved $f into foreman-state/"
    elif [ -e "$old/$f" ]; then
      say "warning: $old/$f and $new/$f both exist; Foreman uses the one in foreman-state/"
    fi
  done
}

# reduce_unit UNIT KEEP OUT: UNIT with only the sandbox lines whose key matches the awk regex
# KEEP (an empty KEEP drops the whole block, markers too).
reduce_unit() {
  if [ -z "$2" ]; then
    awk '/^# sandbox:begin/ { on = 1 } !on { print } /^# sandbox:end/ { on = 0 }' "$1" >"$3"
  else
    awk -v keep="$2" '/^# sandbox:begin/ { on = 1; print; next } /^# sandbox:end/ { on = 0 }
      !on || $0 ~ /^#/ || $0 ~ keep { print }' "$1" >"$3"
  fi
}

# foreman_unit UNIT: install the unit, sandboxed if this user manager can apply the sandbox.
# When it cannot apply the whole sandbox, the credentials and the user manager are kept
# out of reach with as much of it as it takes: first the InaccessiblePaths lines with
# PrivateUsers, then the InaccessiblePaths lines alone; only if neither applies does the unit
# go in with no sandbox.
foreman_unit() {
  local unit=$1 u=$HOME/.config/systemd/user pem=$HOME/.config/factory/github-app.pem f keep applied=""
  mkdir -p "$u"
  cp "$unit" "$D/foreman.service"
  if local_mode; then
    printf 'would run: systemd-run --user --wait --pipe %s true\n' "$(sandbox_lines "$unit" | sed 's/^/-p /' | tr '\n' ' ')" >&2
  elif ! command -v systemd-run >/dev/null 2>&1; then
    say "WARNING: no systemd-run here to apply Foreman's sandbox; installing foreman.service without it" \
      "(it can then read the App key and the Claude token)"
    reduce_unit "$unit" "" "$D/foreman.service"
  else
    for keep in full '^(PrivateUsers|InaccessiblePaths)=' '^InaccessiblePaths='; do
      if [ "$keep" = full ]; then cp "$unit" "$D/foreman.try"; else reduce_unit "$unit" "$keep" "$D/foreman.try"; fi
      if in_sandbox "$D/foreman.try" true >/dev/null 2>&1; then applied=$keep; break; fi
    done
    case "$applied" in
      full) ;;
      "")
        say "WARNING: this user manager cannot apply Foreman's sandbox, not even its InaccessiblePaths;" \
          "installing foreman.service without it (it can then read the App key and the Claude token)"
        reduce_unit "$unit" "" "$D/foreman.service" ;;
      *)
        mv "$D/foreman.try" "$D/foreman.service"
        say "WARNING: this user manager cannot apply Foreman's whole sandbox; installing it with only" \
          "$(sandbox_lines "$D/foreman.service" | sed 's/=.*//' | sort -u | tr '\n' ' ')(the" \
          "credentials and the user manager stay out of reach; the binaries, source and config do not)" ;;
    esac
    rm -f "$D/foreman.try"
  fi
  if [ -n "$applied" ]; then
    unit=$D/foreman.service # what goes in, checked under its own sandbox
    if [ -e "$pem" ]; then
      if in_sandbox "$unit" cat "$pem" >/dev/null 2>&1; then
        say "SECURITY WARNING: under its sandbox Foreman can still read $pem"
      else
        say "sandbox: the App key is out of Foreman's reach"
      fi
    else
      say "sandbox: applies (no App key here yet to check)"
    fi
    # the user manager runs whatever it is asked to, outside any sandbox: Foreman must not reach it
    if in_sandbox "$unit" systemd-run --user --wait --pipe --quiet --collect true >/dev/null 2>&1; then
      say "SECURITY WARNING: under its sandbox Foreman can still reach the user manager (systemd-run --user)"
    else
      say "sandbox: the user manager is out of Foreman's reach"
    fi
  fi
  install_file "$D/foreman.service" "$u/foreman.service" 644
  # Foreman wakes the tick, and starts the screen relay for Live, through files these units
  # watch: neither runs inside its sandbox
  for f in factory-wake.path factory-wake.service factory-screens.path factory-screens.service; do
    [ -f "$D/out/systemd/$f" ] && install_file "$D/out/systemd/$f" "$u/$f" 644
  done
}

# backup_units: the daily backup and Sunday verify timers; enabled once the backup key
# exists (never written here).
backup_units() {
  local u=$HOME/.config/systemd/user f
  for f in factory-backup.service factory-backup.timer factory-backup-verify.service factory-backup-verify.timer; do
    [ -f "$D/out/systemd/$f" ] && install_file "$D/out/systemd/$f" "$u/$f" 644
  done
  return 0
}

# system_foreman: host/foreman-system.sh has made Foreman a system unit on this host.
system_foreman() { ! local_mode && [ -f /etc/systemd/system/foreman.service ]; }

# foreman_env UNIT: Foreman's arguments, as the rendered user unit gives them, for the system unit
# (host/foreman-system.sh) to read. Rewritten only when they change: a rewrite restarts Foreman.
foreman_env() {
  local args
  args=$(sed -n 's|^ExecStart=%h/factory/bin/foreman ||p' "$1" | sed "s|%h|$HOME|g")
  [ -n "$args" ] || { say "warning: no Foreman arguments in $1: foreman.env not written"; return 0; }
  printf 'FOREMAN_ARGS="%s"\n' "$args" >"$F/foreman.env.new"
  if cmp -s "$F/foreman.env.new" "$F/foreman.env" 2>/dev/null; then rm -f "$F/foreman.env.new"; else mv "$F/foreman.env.new" "$F/foreman.env"; fi
}

foreman_health() {
  local u=$HOME/.config/systemd/user/foreman.service listen body="" i
  local_mode && return 0
  if system_foreman; then
    # its path unit restarts it after the new binary lands; a stop can take up to 10 s
    sleep 2
    for i in $(seq 1 20); do [ "$(systemctl show -P ActiveState foreman 2>/dev/null)" = active ] && break; sleep 1; done
    systemctl -q is-active foreman 2>/dev/null || { say "warning: the system foreman unit is not active"; return 0; }
    u=$F/foreman.env
  else
    sysq systemctl --user -q is-active foreman || return 0
  fi
  command -v curl >/dev/null 2>&1 || { say "no curl here: Foreman's /healthz not checked"; return 0; }
  listen=$(tr -d '"' <"$u" | awk '{ for (i = 1; i < NF; i++) if ($i == "--listen") { print $(i + 1); exit } }')
  case "$listen" in
    0.0.0.0:*) listen=127.0.0.1:${listen#0.0.0.0:} ;;
    "[::]:"*) listen="[::1]:${listen#\[::\]:}" ;;
  esac
  for i in 1 2 3 4 5 6 7 8 9 10; do
    body=$(curl -fsS --max-time 3 "http://$listen/healthz" 2>/dev/null) && break
    sleep 1
  done
  case "$body" in
    *exposed:*) say "SECURITY WARNING: Foreman's /healthz: $(printf '%s' "$body" | tr '\n' ' ')" ;;
    ok*) say "Foreman's /healthz: ok, no sealed file in reach" ;;
    *) say "warning: Foreman did not answer /healthz on $listen" ;;
  esac
}

v_install() {
  local c=$1 cc=${2:-} n=$F/bin/.new o=$D/out b t name u=$HOME/.config/systemd/user
  foreman_was_active=0
  word "$c"
  case "$cc" in *[!0-9a-f-]*) die "$cc is not a config commit" ;; esac
  check_build "$n" factory foreman factory-remote
  [ -d "$o" ] || die "nothing staged"
  take_tick_lock

  # the binaries, from the explicit list; pi-run from the source tree
  for b in factory foreman factory-remote; do install_bin "$n/$b" "$F/bin/$b"; done
  install_file "$n/BUILD" "$F/bin/BUILD" 644
  install_file "$F/src/$c/bin/pi-run" "$F/bin/pi-run" 755
  rm -rf "$F/bin/render" "$F/bin/foreman.new" "$F/bin/__pycache__" "$F/patch_provision.py" "$n"
  ln -sfn "$F/bin/factory" "$HOME/.local/bin/factory"
  ln -sfn "$F/bin/pi-run" "$HOME/.local/bin/pi-run"

  # config, context, units, images and templates, as factory render made them
  [ ! -e "$F/config.toml" ] || cp -p "$F/config.toml" "$F/config.toml.prev"
  install_file "$o/config.toml" "$F/config.toml"
  if [ -d "$D/config/context" ]; then
    rm -rf "$F/context.new"
    cp -R "$D/config/context" "$F/context.new"
    rm -rf "$F/context"
    mv "$F/context.new" "$F/context"
  fi
  # the config commit, as deploy.sh read it from the config checkout; - when it knows none
  case "$cc" in
    '') ;;
    -) rm -f "$F/deploys/config-commit" ;;
    *)
      mkdir -p -m 700 "$F/deploys"
      printf '%s\n' "$cc" >"$F/deploys/config-commit.tmp"
      mv -f "$F/deploys/config-commit.tmp" "$F/deploys/config-commit" ;;
  esac
  cp "$o"/systemd/* "$F/systemd/"
  cp -R "$o/images/." "$F/images/"
  for t in "$o"/scion-templates/*/; do
    name=$(basename "$t")
    mkdir -p "$HOME/.scion/templates/$name"
    cp -R "$t." "$HOME/.scion/templates/$name/"
  done
  exec 9>&- # the tick may run again

  # Foreman's user unit and state; the backup timers
  mkdir -p "$u"
  move_foreman_state
  foreman_env "$o/systemd/foreman.service"
  if system_foreman; then
    # host/foreman-system.sh moved it to a sandboxed system unit, which its path unit restarts
    # when bin/foreman or foreman.env changes; the user unit must stay off (same address)
    say "Foreman runs as a system unit (host/foreman-system.sh); it restarts on the new binary"
    for t in factory-wake.path factory-wake.service factory-screens.path factory-screens.service; do
      [ -f "$o/systemd/$t" ] && install_file "$o/systemd/$t" "$u/$t" 644
    done
  else
    foreman_unit "$o/systemd/foreman.service"
  fi
  backup_units
  sysd systemctl --user daemon-reload || say "warning: systemctl --user daemon-reload failed"
  if [ -f "$u/factory-wake.path" ]; then
    sysd systemctl --user enable --now factory-wake.path || say "warning: factory-wake.path did not start: Foreman's wakes wait for the next timer tick"
  fi
  if [ -f "$u/factory-screens.path" ]; then
    sysd systemctl --user enable --now factory-screens.path || say "warning: factory-screens.path did not start: Live shows the agents' logs, not their screens"
  fi
  if ! system_foreman && { sysq systemctl --user -q is-enabled foreman || [ $foreman_was_active = 1 ]; }; then
    sysd systemctl --user restart foreman || say "warning: foreman did not restart"
  fi
  if [ -f "$HOME/.config/factory/backup-key" ]; then
    sysd systemctl --user enable --now factory-backup.timer factory-backup-verify.timer || say "warning: the backup timers did not start"
  else
    say "backup: no key yet (run factory backup keygen); the timers stay off until then"
  fi
  foreman_health
}

# helper_bundle: what a helper host gets, from what this host just installed.
# The claude image's context goes without sciontool (23 MB): a Claude Code
# helper keeps its own copy there.
v_helper_bundle() {
  local claude=()
  [ ! -d "$F/images/scion-agent" ] || claude=(images/scion-agent)
  tar -C "$F" --exclude=images/scion-agent/sciontool -cf - bin/factory-remote bin/BUILD bin/pi-run config.toml systemd images/pi "${claude[@]}"
}

# helper_install: a helper runs no dispatcher code: factory-remote, and bin/factory a link to it.
v_helper_install() {
  local x=$D/helper
  mkdir -p "$F/bin" "$F/systemd" "$F/images" "$HOME/.local/bin"
  rm -rf "$x"
  mkdir -p "$x"
  tar -C "$x" --no-same-owner -xf -
  check_build "$x/bin" factory-remote
  install_bin "$x/bin/factory-remote" "$F/bin/factory-remote"
  install_file "$x/bin/BUILD" "$F/bin/BUILD" 644
  install_file "$x/bin/pi-run" "$F/bin/pi-run" 755
  ln -sfn factory-remote "$F/bin/.factory.deploy.tmp"
  mv -fT "$F/bin/.factory.deploy.tmp" "$F/bin/factory"
  rm -rf "$F/bin/render" "$F/bin/foreman" "$F/bin/foreman.new" "$F/bin/factory.prev" "$F/bin/__pycache__" "$F/patch_provision.py"
  ln -sfn "$F/bin/factory" "$HOME/.local/bin/factory"
  ln -sfn "$F/bin/pi-run" "$HOME/.local/bin/pi-run"
  [ ! -e "$F/config.toml" ] || cp -p "$F/config.toml" "$F/config.toml.prev"
  install_file "$x/config.toml" "$F/config.toml"
  cp "$x"/systemd/* "$F/systemd/"
  mkdir -p "$F/images/pi"
  cp -R "$x/images/pi/." "$F/images/pi/"
  if [ -d "$x/images/scion-agent" ]; then
    mkdir -p "$F/images/scion-agent"
    cp -R "$x/images/scion-agent/." "$F/images/scion-agent/"
  fi
  rm -rf "$D"
}

# finish: keep this source tree and the newest other one, and clear the staging area.
v_finish() {
  local c=$1 d keep=""
  word "$c"
  for d in $(ls -1t "$F/src"); do
    [ "$d" = "$c" ] && continue
    [ -d "$F/src/$d" ] || continue
    if [ -z "$keep" ]; then keep=$d; continue; fi
    rm -rf "${F:?}/src/$d"
  done
  rm -rf "$D"
}

verb=${1:?a verb}
shift
mkdir -p "$D"
chmod 700 "$D"
case "$verb" in
  prepare) v_prepare "$@" ;;
  install-go) v_install_go "$@" ;;
  build) v_build "$@" ;;
  receive) v_receive ;;
  build-id) v_build_id ;;
  reuse) v_reuse "$@" ;;
  running) v_running ;;
  candidate) v_candidate "$@" ;;
  stage) v_stage ;;
  install) v_install "$@" ;;
  helper-bundle) v_helper_bundle ;;
  helper-install) v_helper_install ;;
  finish) v_finish "$@" ;;
  *) die "unknown verb $verb" ;;
esac
REMOTE

# on HOST VERB ARGS...: run a verb of the host script there (stdin passes through).
on() {
  local h=$1
  shift
  case "$h" in
    local:*)
      local home=${h#local:}
      env -u FACTORY_ROOT -u FACTORY_API_TOKENS -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u FACTORY_DEPLOY_SELF -u FACTORY_DEPLOY_DIR \
        HOME="$home" FACTORY_DEPLOY_LOCAL=1 FACTORY_DEPLOY_NAME="$h" \
        FACTORY_GOROOT="${FACTORY_GOROOT:-$HOME/.local/share/factory/go/$pin}" \
        FACTORY_GO_CACHE="${FACTORY_GO_CACHE:-$HOME/.cache/factory-go}" \
        bash "$home/factory/.deploy/remote.sh" "$@" ;;
    self)
      FACTORY_DEPLOY_NAME=self FACTORY_DEPLOY_SELF=1 FACTORY_DEPLOY_DIR="$FACTORY_DEPLOY_DIR" \
        bash "$FACTORY_DEPLOY_DIR/remote.sh" "$@" ;;
    *) ssh -o ServerAliveInterval=60 "$h" FACTORY_DEPLOY_NAME="$h" bash factory/.deploy/remote.sh "$@" ;;
  esac
}

# send_script HOST: put the host script in ~/factory/.deploy/remote.sh there (for self, in
# FACTORY_DEPLOY_DIR).
send_script() {
  case "$1" in
    self)
      mkdir -p -m 700 "$FACTORY_DEPLOY_DIR"
      cp "$tmp/remote.sh" "$FACTORY_DEPLOY_DIR/remote.sh" ;;
    local:*)
      local home=${1#local:}
      [ -d "$home" ] || die "no such directory: $home"
      mkdir -p -m 700 "$home/factory/.deploy"
      cp "$tmp/remote.sh" "$home/factory/.deploy/remote.sh" ;;
    *) ssh "$1" 'mkdir -p ~/factory/.deploy && chmod 700 ~/factory/.deploy && cat >~/factory/.deploy/remote.sh' <"$tmp/remote.sh" ;;
  esac
}

# build_here OUT: build the archived commit on this machine with the pinned toolchain.
build_here() {
  local s=$tmp/local-src
  mkdir -m 700 "$s"
  tar -C "$s" -xf "$tmp/src.tar"
  printf '%s\n' "$commit" >"$s/COMMIT"
  "$s/build.sh" --no-box --out "$1" >&2 || die "the local build failed (a missing toolchain: $s/build.sh --install-toolchain TARBALL)"
  check_build "$1"
}

# contains GITDIR RUNNING CANDIDATE: CANDIDATE holds RUNNING, a commit id git knows in GITDIR.
contains() {
  case "$2" in '' | *[!0-9a-f]*) return 1 ;; esac
  git -C "$1" merge-base --is-ancestor "$2^{commit}" "$3" 2>/dev/null
}

# guard_older: refuse a core commit or a config checkout that lacks what the host runs, or that
# git here does not know (an older, diverged or unfetched checkout), unless --allow-older.
guard_older() {
  local running rc rcc
  running=$(on "$host" running) || die "could not read what $host runs"
  read -r rc rcc <<<"$running"
  local fix="update core/ (git submodule update --remote core) or pass --allow-older"
  [ "$host" != self ] || fix="deploy a newer commit of core main, or factory deploy start --allow-older"
  [ "$rc" = - ] || contains "$core" "$rc" "$commit" || die "core $commit does not contain what $host runs ($rc): $fix"
  [ "$host" != self ] && [ "$rcc" != - ] || return 0
  [ -n "$config_commit" ] ||
    die "$config is not a git checkout: $host runs config ${rcc:0:12}, which it cannot check; pass --allow-older"
  contains "$config" "$rcc" "$config_commit" ||
    die "config ${config_commit:0:12} does not contain what $host runs (${rcc:0:12}): pull the config checkout or pass --allow-older"
}

send_bins() { tar -C "$1" -cf - factory foreman factory-remote BUILD | on "$host" receive; }

# ---------------------------------------------------------------- the deploy
say "deploying $commit to $host"
send_script "$host"
[ $dry = 1 ] || [ $allow_older = 1 ] || guard_older
on "$host" prepare "$commit" <"$tmp/src.tar"
if [ -n "$tarball" ]; then
  on "$host" install-go "$commit" "$(basename "$tarball")" <"$tarball"
fi
if [ -n "$prebuilt" ]; then
  send_bins "$prebuilt"
elif [ $verify = 0 ] && [ $dry = 0 ] && on "$host" reuse "$commit"; then
  : # the host runs this commit already: only the config changes
elif [ "$build" = helper ]; then
  on "$host" candidate "$commit"
elif [ "$build" = local ] && [ $verify = 0 ]; then
  build_here "$tmp/out"
  send_bins "$tmp/out"
else
  on "$host" build "$commit"
  if [ $verify = 1 ]; then
    build_here "$tmp/out"
    on "$host" build-id >"$tmp/host-BUILD"
    cmp -s "$tmp/host-BUILD" "$tmp/out/BUILD" || {
      diff "$tmp/host-BUILD" "$tmp/out/BUILD" >&2 || true
      die "the host's build and this machine's differ: nothing installed"
    }
    say "verified: the host's build and this machine's are byte-identical"
  fi
fi
on "$host" stage <"$tmp/config.tar"
if [ "$host" = self ]; then
  on "$host" install "$commit"
else
  on "$host" install "$commit" "${config_commit:--}"
fi
for h in "${helpers[@]}"; do
  say "relaying factory-remote to the helper $h"
  send_script "$h"
  on "$host" helper-bundle | on "$h" helper-install
done
on "$host" finish "$commit"

if [ $dry = 1 ]; then
  home=${host#local:}
  (cd "$home" && find . \( -type f -o -type l \) ! -path './factory/src/*' | sed 's|^\./||' | sort)
  echo "dry run: deployed into $home"
else
  say "deployed $commit to $host"
fi

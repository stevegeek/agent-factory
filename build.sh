#!/usr/bin/env bash
# build.sh: the one build entry point, on the box and anywhere else.
#
#   build.sh [--out DIR] [--commit ID]       build factory, foreman, factory-remote into DIR
#            [--box | --no-box]              on-box mode (scope, tick lock, agent wait); default: auto
#            [--scope | --no-scope]          run inside a systemd user scope; default: on the box only
#            [--force]                       on the box, do not wait for agents to finish
#   build.sh --check-toolchain              only check the pinned toolchain
#   build.sh --install-toolchain TARBALL    install the pinned official tarball (sha-checked)
#   build.sh --measure                      a cold build that records each compile/link's peak RSS
#   build.sh --verify-repro                 build twice in different directories, compare hashes
#   build.sh --test                         go vet ./... and the store/ticket/config tests
#   build.sh --clean                        empty the build cache
#
# It never uses a system go: only $FACTORY_GOROOT (default
# ~/.local/share/factory/go/<pin>), which must report exactly the pinned
# version, with no X: suffix and an empty GOEXPERIMENT.
set -euo pipefail
umask 022

SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
die() { printf 'build.sh: %s\n' "$*" >&2; exit 1; }
say() { printf 'build.sh: %s\n' "$*" >&2; }

# The pin: the toolchain line of go.mod is the single source of truth.
PIN=$(awk '$1 == "toolchain" { print $2 }' "$SRC/go.mod")
[ -n "$PIN" ] || die "go.mod has no toolchain line"
PINS=${FACTORY_GO_PINS:-$SRC/build/go.sha256}

MODE=build OUT="" COMMIT="" BOX=auto SCOPE=auto FORCE=0 TARBALL=""
ARGS=("$@")   # before the loop shifts them away: enter_scope re-runs us with these
while [ $# -gt 0 ]; do
  case "$1" in
    --out) OUT=${2:?--out needs a directory}; shift ;;
    --out=*) OUT=${1#--out=} ;;
    --commit) COMMIT=${2:?--commit needs an id}; shift ;;
    --commit=*) COMMIT=${1#--commit=} ;;
    --box) BOX=1 ;;
    --no-box) BOX=0 ;;
    --scope) SCOPE=1 ;;
    --no-scope) SCOPE=0 ;;
    --force) FORCE=1 ;;
    --check-toolchain) MODE=check ;;
    --install-toolchain) MODE=install; TARBALL=${2:?--install-toolchain needs a tarball}; shift ;;
    --measure) MODE=measure ;;
    --verify-repro) MODE=repro ;;
    --test) MODE=test ;;
    --clean) MODE=clean ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
  shift
done

GOROOT=${FACTORY_GOROOT:-$HOME/.local/share/factory/go/$PIN}
GO=$GOROOT/bin/go
CACHE=${FACTORY_GO_CACHE:-$HOME/.cache/factory-go}
FACTORY_ROOT=${FACTORY_ROOT:-$HOME/factory}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

# A value from build/limits (key=value lines, written by a
# measurement), or the provisional default.
limit() {
  local v=""
  if [ -f "$SRC/build/limits" ]; then
    v=$(awk -F= -v k="$1" '$1 == k { print $2 }' "$SRC/build/limits" | tail -n 1)
  fi
  printf '%s' "${v:-$2}"
}

# ------------------------------------------------------------- toolchain
install_toolchain() {
  [ -f "$TARBALL" ] || die "no such tarball: $TARBALL"
  local name sum
  name=$(basename "$TARBALL")
  case "$name" in
    "$PIN".*.tar.gz) ;;
    *) die "$name is not a $PIN release tarball" ;;
  esac
  sum=$(sha256 "$TARBALL" | awk '{ print $1 }')
  awk -v s="$sum" -v n="$name" '$1 == s && $2 == n { found = 1 } END { exit !found }' "$PINS" ||
    die "$name has sha256 $sum, which is not pinned in $PINS: refusing it"
  [ ! -e "$GOROOT" ] || die "$GOROOT already exists: remove it first to reinstall"
  local parent
  parent=$(dirname "$GOROOT")
  mkdir -p "$parent"
  tmp=$(mktemp -d "$parent/.install.XXXXXX")
  trap 'rm -rf "${tmp:-}"' EXIT
  tar -xzf "$TARBALL" -C "$tmp"
  [ -x "$tmp/go/bin/go" ] || die "$name holds no go/bin/go"
  mv "$tmp/go" "$GOROOT"
  rm -rf "$tmp"
  trap - EXIT
  goq telemetry off >/dev/null 2>&1 || true
  say "installed $PIN in $GOROOT"
}

# goq runs the pinned go with none of the caller's go settings.
goq() {
  env -u GOEXPERIMENT -u GOFLAGS -u GOOS -u GOARCH GOENV=off GOTOOLCHAIN=local GOROOT="$GOROOT" "$GO" "$@"
}

check_toolchain() {
  [ -x "$GO" ] || die "no pinned toolchain at $GOROOT (a system go is never used): run build.sh --install-toolchain $PIN.<os>-<arch>.tar.gz"
  local v e
  # The caller's GOEXPERIMENT, GOFLAGS and go env file never count.
  v=$(goq env GOVERSION) || die "$GO env GOVERSION failed"
  case "$v" in
    *X:*) die "toolchain $v is a GOEXPERIMENT build (X:): only the official $PIN is allowed" ;;
  esac
  [ "$v" = "$PIN" ] || die "toolchain at $GOROOT is $v, not the pinned $PIN"
  e=$(goq env GOEXPERIMENT) || die "$GO env GOEXPERIMENT failed"
  [ -z "$e" ] || die "toolchain at $GOROOT has GOEXPERIMENT=$e: only the official $PIN is allowed"
}

# The environment of every go invocation. Nothing from the caller's
# GOFLAGS or GOEXPERIMENT survives.
go_env() {
  unset GOEXPERIMENT GOFLAGS GOOS GOARCH GOAMD64 GOPATH GOWORK GOENV
  export GOROOT PATH="$GOROOT/bin:/usr/bin:/bin"
  export GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOENV=off GOWORK=off
  export GOFLAGS="-p=1 -trimpath -buildvcs=false -mod=readonly"
  export CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1
  local memlimit=""
  [ -f "$SRC/build/limits.gomemlimit" ] && memlimit=$(tr -d ' \n' <"$SRC/build/limits.gomemlimit")
  export GOMAXPROCS=2 GOGC=50 GOMEMLIMIT=${memlimit:-300MiB}
  export GOCACHE=${GOCACHE_OVERRIDE:-$CACHE/build} GOMODCACHE=$CACHE/mod GOTMPDIR=$CACHE/tmp
  mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOTMPDIR"
}

# ------------------------------------------------------------- the box
on_box() {
  case "$BOX" in
    1) return 0 ;;
    0) return 1 ;;
  esac
  [ -d "$FACTORY_ROOT/board" ] && [ -f "$FACTORY_ROOT/config.toml" ]
}

# The source must not be agent-writable: owned by us, mode 0700.
check_source() {
  local owner mode
  if stat -c '%u' "$SRC" >/dev/null 2>&1; then
    owner=$(stat -c '%u' "$SRC") mode=$(stat -c '%a' "$SRC")
  else
    owner=$(stat -f '%u' "$SRC") mode=$(stat -f '%Lp' "$SRC")
  fi
  [ "$owner" = "$(id -u)" ] || die "$SRC is not owned by $(id -un): refusing to build it"
  [ "$mode" = "700" ] || die "$SRC has mode $mode, not 700: refusing to build it"
}

# Re-run inside a systemd user scope with the build's memory cap, then raise
# our own OOM score so the kernel kills the build before any agent.
enter_scope() {
  if [ "${FACTORY_BUILD_SCOPED:-}" != 1 ]; then
    command -v systemd-run >/dev/null 2>&1 || die "systemd-run not found: use --no-scope"
    export FACTORY_BUILD_SCOPED=1
    exec systemd-run --user --scope --quiet \
      -p MemoryHigh="$(limit memory_high 400M)" -p MemoryMax="$(limit memory_max 500M)" -p CPUWeight=20 \
      nice -n 10 "$SRC/build.sh" "${ARGS[@]}"
  fi
  echo 1000 >/proc/self/oom_score_adj 2>/dev/null || say "could not raise oom_score_adj"
}

# Hold the tick lock (so no agent or gate starts) and wait until no factory
# container runs and memory is free, for up to two hours.
wait_for_quiet() {
  exec 9>>"$FACTORY_ROOT/.tick.lock"
  say "waiting for the tick lock"
  flock 9
  [ "$FORCE" = 1 ] && return 0
  local need deadline
  need=$(( $(limit memory_max 500M | tr -dc 0-9) + 200 ))
  deadline=$(( $(date +%s) + 7200 ))
  while :; do
    local busy="" avail
    # The factory user runs only factory containers (Scion agents, gates,
    # merge checks, smoke tests), so any running container means busy.
    if command -v podman >/dev/null 2>&1; then
      busy=$(podman ps --format '{{.Names}}' 2>/dev/null | tr '\n' ' ' || true)
      busy=${busy% }
    fi
    avail=$(awk '$1 == "MemAvailable:" { print int($2 / 1024) }' /proc/meminfo)
    if [ -z "$busy" ] && [ "$avail" -ge "$need" ]; then
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      die "gave up after 2 hours: containers running (${busy:-none}) or only ${avail} MB free (need $need); the old binaries stay"
    fi
    sleep 30
  done
}

# ------------------------------------------------------------- building
# The commit id: --commit, else the COMMIT file deploy writes beside the
# archived source, else "unknown". build.sh never runs git itself.
commit_id() {
  if [ -n "$COMMIT" ]; then printf '%s' "$COMMIT"; return; fi
  if [ -f "$SRC/COMMIT" ]; then tr -d ' \n' <"$SRC/COMMIT"; return; fi
  printf 'unknown'
}

# build SRC_DIR OUT_DIR [extra go build flags...]
build_into() {
  local src=$1 out=$2; shift 2
  local commit pkg
  commit=$(commit_id)
  case "$commit" in *[!A-Za-z0-9._-]*) die "commit id $commit is not one word" ;; esac
  pkg=github.com/stevegeek/agent-factory/internal/buildinfo
  mkdir -p "$out"
  (cd "$src" && "$GO" build "$@" -ldflags="-s -w -X $pkg.Version=$commit -X $pkg.GoVersion=$PIN" \
    -gcflags=all=-c=1 -o "$out/" ./cmd/...)
  (cd "$out" && sha256 factory foreman factory-remote >BUILD.tmp && echo "commit $commit go $("$GO" env GOVERSION)" >>BUILD.tmp && mv BUILD.tmp BUILD)
}

measure() {
  local log peak_mb limit_mb
  tmp=$(mktemp -d)
  trap 'rm -rf "${tmp:-}"' EXIT
  log=$tmp/measure.log
  : >"$log"
  export FACTORY_MEASURE_LOG=$log
  GOCACHE_OVERRIDE=$tmp/cache go_env # a cold build
  local start end
  start=$(date +%s)
  build_into "$SRC" "$tmp/out" -toolexec="$SRC/build/measure-tool"
  end=$(date +%s)
  limit_mb=$(limit peak_mb 450)
  peak_mb=$(awk '{ if ($1 > m) m = $1 } END { print int(m / 1024) }' "$log")
  printf 'largest processes (peak RSS MB, tool, package):\n'
  sort -rn "$log" | awk 'NR <= 5 { printf "  %6d  %s %s\n", $1 / 1024, $2, $3 }'
  printf 'processes: %s, cold build: %ss, peak: %s MB, limit: %s MB\n' "$(wc -l <"$log" | tr -d ' ')" $((end - start)) "$peak_mb" "$limit_mb"
  [ "$peak_mb" -le "$limit_mb" ] || die "peak RSS $peak_mb MB is over the limit of $limit_mb MB"
}

repro() {
  local a b
  tmp=$(mktemp -d)
  trap 'rm -rf "${tmp:-}"' EXIT
  for side in a b; do
    mkdir -p "$tmp/$side/src"
    tar -C "$SRC" --exclude=.git --exclude=./foreman -cf - . | tar -C "$tmp/$side/src" -xf -
    GOCACHE_OVERRIDE=$tmp/$side/cache go_env
    build_into "$tmp/$side/src" "$tmp/$side/out"
  done
  a=$(cat "$tmp/a/out/BUILD") b=$(cat "$tmp/b/out/BUILD")
  if [ "$a" != "$b" ]; then
    diff <(echo "$a") <(echo "$b") >&2 || true
    die "the two builds differ: not reproducible"
  fi
  printf '%s\n' "$a"
  say "reproducible: both builds are byte-identical"
}

case "$MODE" in
  install) install_toolchain; check_toolchain; exit 0 ;;
  clean) rm -rf "$CACHE/build" "$CACHE/tmp"; say "emptied $CACHE"; exit 0 ;;
esac

check_toolchain
[ "$MODE" = check ] && { say "toolchain ok: $PIN at $GOROOT"; exit 0; }
COMMIT=$(commit_id)

if on_box; then
  check_source
  [ "$SCOPE" = 0 ] || enter_scope
  wait_for_quiet
elif [ "$SCOPE" = 1 ]; then
  enter_scope
fi

case "$MODE" in
  build)
    go_env
    OUT=${OUT:-$CACHE/out}
    build_into "$SRC" "$OUT"
    cat "$OUT/BUILD"
    ;;
  test)
    go_env
    export GOOS="$("$GO" env GOHOSTOS)" GOARCH="$("$GO" env GOHOSTARCH)"
    (cd "$SRC" && "$GO" vet ./... && "$GO" test $(for p in store ticket config; do [ -d "internal/$p" ] && echo "./internal/$p/..."; done))
    ;;
  measure) measure ;;
  repro) repro ;;
esac

#!/usr/bin/env bash
# Build the factory worker image for Lever on top of the Lever Claude base:
#   ./build.sh
# Overrides (environment): LEVER_BASE (default lever-claude-slim:amd64),
# FACTORY_WORKER_TAG (default lever.local/factory-worker:amd64), and the pins
# CRYSTAL_VERSION / AMEBA_VERSION (the factory's [images] crystal / ameba) and
# GIT_VERSION + GIT_SHA256 and GO_VERSION + GO_SHA256; unset ones keep the
# Dockerfile's defaults.
# Keep this directory outside the Lever instance tree.
set -euo pipefail
cd "$(dirname "$0")"
ARCH=amd64 # the Crystal tarball used here is x86_64 only
BASE="${LEVER_BASE:-lever-claude-slim:${ARCH}}"
TAG="${FACTORY_WORKER_TAG:-lever.local/factory-worker:${ARCH}}"
docker image inspect "$BASE" >/dev/null || { echo "missing $BASE: build the Lever base image first (make lever-image-slim in the lever clone)" >&2; exit 1; }
got=$(docker image inspect --format '{{.Architecture}}' "$BASE")
[ "$got" = "$ARCH" ] || { echo "$BASE is $got, want $ARCH" >&2; exit 1; }

args=(--build-arg LEVER_BASE="$BASE")
for v in CRYSTAL_VERSION AMEBA_VERSION GIT_VERSION GIT_SHA256 GO_VERSION GO_SHA256; do
  if [ -n "${!v:-}" ]; then args+=(--build-arg "$v=${!v}"); fi
done
docker build --platform "linux/${ARCH}" "${args[@]}" -t "$TAG" .

# Smoke test, as the image's normal user (never run claude here). --entrypoint
# bash skips sciontool's init, which prints errors outside scion.
docker run --rm --entrypoint bash "$TAG" -euc '
  echo "user: $(id -un)"
  ruby -v
  bundle -v
  crystal --version | head -1
  shards --version
  ameba --version
  go version
  echo "git: $(command -v git) $(git --version)"
  v=$(git --version | sed "s/^git version //")
  printf "%s\n2.48\n" "$v" | sort -V -C -r || { echo "git $v is older than 2.48" >&2; exit 1; }

  t=$(mktemp -d)
  # A gem installs into a writable GEM_HOME (built locally: no network needed).
  mkdir -p "$t/gem/lib" && cd "$t/gem"
  echo "module Smoke; end" > lib/smoke.rb
  cat > smoke.gemspec <<EOF
Gem::Specification.new { |s| s.name = "smoke"; s.version = "0.0.1"; s.summary = "smoke"; s.authors = ["factory"]; s.files = ["lib/smoke.rb"] }
EOF
  gem build -q smoke.gemspec >/dev/null 2>&1
  GEM_HOME="$t/gems" gem install -q --local --no-document smoke-0.0.1.gem
  GEM_HOME="$t/gems" ruby -e "require %q(smoke); puts %q(gem install: ok)"

  # A tiny Crystal program compiles and runs.
  cd "$t" && echo "puts \"crystal build: ok\"" > hello.cr
  crystal build -o hello hello.cr && ./hello
  # ...and the git bundle round trip job.sh does.
  git init -q -b main src
  git -C src -c user.name=s -c user.email=s@localhost commit -q --allow-empty -m one
  git -C src bundle create -q ../repo.bundle refs/heads/main
  git -c init.defaultBranch=main clone -q --no-checkout repo.bundle repo
  git -C repo checkout -q -B bot/smoke origin/main
  git -C repo bundle create -q ../out.bundle refs/heads/bot/smoke
  git -C repo bundle verify -q ../out.bundle
  echo "git bundle: ok"
  rm -rf "$t"
'
docker image inspect --format "$TAG: {{.Size}} bytes" "$TAG"

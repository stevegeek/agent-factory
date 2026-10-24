# Factory worker image for Lever: the Lever Claude base plus the factory's
# toolchain for Ruby gems, Crystal shards and Go modules (the same tools as
# images/claude/Containerfile, without the Claude Code and Scion bits the base
# already has). A worker runs factory jobs with job.sh (see factory-worker.md):
# git for the bundle round trip, then the job's own install/test/lint commands.
# Build with build.sh. Nothing goes under /home/scion: scion mounts the agent's
# home over it at runtime.
ARG LEVER_BASE=lever-claude-slim:amd64
FROM ${LEVER_BASE}
USER root
ENV DEBIAN_FRONTEND=noninteractive

# Ruby and Bundler, and the headers gems with native extensions need.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ruby ruby-dev ruby-bundler build-essential pkg-config curl ca-certificates \
      libyaml-dev libsqlite3-dev libffi-dev zlib1g-dev procps less \
 && rm -rf /var/lib/apt/lists/*

# git >= 2.48 (factory worktrees use extensions.relativeworktrees). Debian 13's
# git is 2.47, so build a pinned release from kernel.org into /usr/local, which
# comes before /usr/bin on PATH. GIT_SHA256 is the release's sha256sums.asc entry.
ARG GIT_VERSION=2.53.0
ARG GIT_SHA256=5818bd7d80b061bbbdfec8a433d609dc8818a05991f731ffc4a561e2ca18c653
RUN apt-get update \
 && apt-get install -y --no-install-recommends libcurl4-openssl-dev libexpat1-dev libssl-dev xz-utils \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSL --retry 3 -o /tmp/git.tar.xz "https://mirrors.edge.kernel.org/pub/software/scm/git/git-${GIT_VERSION}.tar.xz" \
 && echo "${GIT_SHA256}  /tmp/git.tar.xz" | sha256sum -c - \
 && mkdir /tmp/git && tar -xJf /tmp/git.tar.xz -C /tmp/git --strip-components=1 \
 && make -C /tmp/git -j"$(nproc)" prefix=/usr/local NO_TCLTK=1 NO_GETTEXT=1 INSTALL_SYMLINKS=1 all install >/dev/null \
 && rm -rf /tmp/git /tmp/git.tar.xz \
 && /usr/local/bin/git --version

# Crystal, for Crystal projects: the official tarball (its bin/crystal wrapper
# finds the embedded libgc) and ameba built from source. Pinned to the factory's
# [images] crystal / ameba; build.sh passes overrides.
ARG CRYSTAL_VERSION=1.20.1
ARG AMEBA_VERSION=1.7.0
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      gcc libpcre2-dev libevent-dev libssl-dev libxml2-dev libgmp-dev \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /opt/crystal \
 && curl -fsSL "https://github.com/crystal-lang/crystal/releases/download/${CRYSTAL_VERSION}/crystal-${CRYSTAL_VERSION}-1-linux-x86_64.tar.gz" \
    | tar -xz -C /opt/crystal --strip-components=1 \
 && ln -s /opt/crystal/bin/crystal /opt/crystal/bin/shards /usr/local/bin/ \
 && git clone -q --depth 1 -b "v${AMEBA_VERSION}" https://github.com/crystal-ameba/ameba /tmp/ameba \
 && cd /tmp/ameba && shards build ameba --release --no-debug --without-development \
 && install -m 755 bin/ameba /usr/local/bin/ameba \
 && cd / && rm -rf /tmp/ameba /root/.cache

# Go, for the factory's own core: the official tarball at the toolchain build.sh
# pins (go.mod's toolchain line; GO_SHA256 is build/go.sha256's linux-amd64
# line, which internal/archtest checks). No module downloads (GOPROXY=off) and
# no toolchain switch; the build cache starts empty in each fresh container.
ARG GO_VERSION=1.27.1
ARG GO_SHA256=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445
RUN curl -fsSL --retry 3 -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
 && echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/go.tar.gz -C /usr/local \
 && rm /tmp/go.tar.gz \
 && /usr/local/go/bin/go version
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOPROXY=off \
    GOCACHE=/tmp/go-build GOPATH=/tmp/go GOTELEMETRY=off

ENV BUNDLE_JOBS=2 MAKEFLAGS=-j2
# gems go in the worker's own home (scion mounts a writable one over /home/scion), as the
# factory's Scion templates set it; the system gem dir is root's
ENV BUNDLE_PATH=/home/scion/.bundle-cache
LABEL git_version=${GIT_VERSION} go_version=${GO_VERSION} crystal_version=${CRYSTAL_VERSION} ameba_version=${AMEBA_VERSION}
USER scion

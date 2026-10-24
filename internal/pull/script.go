package pull

import (
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// ScriptName is the script factory-pull writes into each job directory.
const ScriptName = "job.sh"

// jobScript is what a helper's job script does around its agent (helper's
// jobScript), split in two for an agent that runs it itself inside its
// Lever container: `prepare` turns repo.bundle into repo/ on the job's
// branch, `finish` writes head (and out.bundle) from it. factory-pull never
// runs git on job data; the worker does, in its jail. The template is
// factory-pull's own; the values filled in are the job's checked meta
// (jobproto.CheckMeta's names: letters, digits and ._/-) and the
// configured identity (letters, digits, spaces and ._@+-), all inside
// single quotes.
const jobScript = `#!/bin/sh
# Written by factory-pull for job @ID@ from its own template (never from the job's data).
#   sh job.sh prepare   check out repo/ on the job's branch
#   sh job.sh finish    write head (and out.bundle) from repo/; then write report.json yourself
set -eu
JOB=$(cd "$(dirname "$0")" && pwd -P)
BRANCH='@BRANCH@'
START='@START@'
BUNDLE='@BUNDLE@'
GIT_NAME='@GIT_NAME@'
GIT_EMAIL='@GIT_EMAIL@'
cd "$JOB"
@DEPS@
case "${1:-}" in
prepare)
  if [ -e repo ]; then echo "job.sh: $JOB/repo already exists" >&2; exit 1; fi
  if [ -f repo.bundle ]; then
    git -c init.defaultBranch=main clone -q --no-checkout repo.bundle repo
    cd repo
    git checkout -q -B "$BRANCH" "origin/$START"
  else
    # a task with no project: scratch space, a repository only so the head check works
    mkdir repo
    cd repo
    git init -q -b "$BRANCH"
    git -c user.name=job -c user.email=job@localhost commit -q --allow-empty -m scratch
  fi
  if [ -n "$GIT_NAME" ]; then git config user.name "$GIT_NAME"; fi
  if [ -n "$GIT_EMAIL" ]; then git config user.email "$GIT_EMAIL"; fi
  if [ -d "$JOB/untracked" ]; then cp -a "$JOB/untracked/." .; fi
  deps || echo "job.sh: the dependencies did not install; see $JOB/deps.log" >&2
  echo "job.sh: $JOB/repo is on $BRANCH"
  ;;
finish)
  cd repo
  git rev-parse "refs/heads/$BRANCH" > "$JOB/head.tmp"
  mv "$JOB/head.tmp" "$JOB/head"
  if [ "$BUNDLE" = 1 ]; then
    git bundle create -q "$JOB/out.bundle.tmp" "refs/heads/$BRANCH"
    mv "$JOB/out.bundle.tmp" "$JOB/out.bundle"
  fi
  echo "job.sh: wrote $JOB/head"
  ;;
*)
  echo "usage: sh job.sh prepare|finish" >&2
  exit 2
  ;;
esac
`

// GateScriptName is the script factory-pull writes into a gate job's
// directory instead of job.sh.
const GateScriptName = "gate.sh"

// gateScript is a gate job's whole run, so that the worker only starts it:
// a fresh clone of repo.bundle with the gated commit checked out detached
// (no branch, no identity, nothing of the job's but its commands), the
// pinned untracked files copied in, then checks.sh (the project's "set -e;
// install; test; lint") with its output in agent.log and a time limit
// under factory-pull's own. It writes report.json last, through a temp
// file: {"status": "done", "gate_exit": N} when the checks ran to an exit,
// {"status": "error", ...} when they could not run or ran out of time, which
// the dispatcher takes as a lost gate job and runs again. The values filled
// in are the job's id and checked meta (a commit id; a number).
const gateScriptText = `#!/bin/sh
# Written by factory-pull for gate job @ID@ from its own template (never from the job's data).
#   sh gate.sh    Gate 1's checks on a fresh checkout of the gated commit; it writes report.json itself
set -u
JOB=$(cd "$(dirname "$0")" && pwd -P)
HEAD='@HEAD@'
LIMIT='@LIMIT@'
cd "$JOB" || exit 1
report() {
  printf '%s\n' "$1" > "$JOB/report.json.tmp" && mv "$JOB/report.json.tmp" "$JOB/report.json"
}
fail() {
  report "{\"status\": \"error\", \"summary\": \"gate.sh: $1\"}"
  exit 1
}
@DEPS@
if [ -e report.json ]; then echo "gate.sh: this job has run already" >&2; exit 1; fi
rm -rf repo
git -c init.defaultBranch=main clone -q --no-checkout repo.bundle repo >> agent.log 2>&1 || fail "could not clone the bundle"
cd repo || fail "no checkout"
git -c advice.detachedHead=false checkout -q --detach "$HEAD" >> "$JOB/agent.log" 2>&1 || fail "could not check out the gated commit"
if [ -d "$JOB/untracked" ]; then cp -a "$JOB/untracked/." . || fail "could not copy the pinned files"; fi
# dependencies that will not install are not a failed gate: the job is run again
deps || fail "the dependencies did not install (deps.log)"
if command -v timeout > /dev/null 2>&1; then
  timeout -k 30 "$LIMIT" sh "$JOB/checks.sh" > "$JOB/agent.log" 2>&1 < /dev/null
  code=$?
  if [ "$code" -eq 124 ] || [ "$code" -eq 137 ]; then fail "the checks ran out of time ($LIMIT s)"; fi
else
  sh "$JOB/checks.sh" > "$JOB/agent.log" 2>&1 < /dev/null
  code=$?
fi
report "{\"status\": \"done\", \"gate_exit\": $code}"
echo "gate.sh: the checks exited $code"
`

// gateScript is a gate job's gate.sh. The caller has checked meta
// (ParseMeta: Start is a commit id). The checks' limit leaves two minutes
// of the job's time for the report.
func gateScript(id string, meta jobproto.Meta) []byte {
	limit := max(60, meta.TimeoutMinutes*60-120)
	return []byte(strings.NewReplacer("@ID@", id, "@HEAD@", meta.Start, "@LIMIT@", strconv.Itoa(limit), "@DEPS@", jobproto.DepsFunc).Replace(gateScriptText))
}

// script is the job's job.sh. The caller has checked meta (ParseMeta) and
// the identity (LoadConfig).
func script(id string, meta jobproto.Meta, gitName, gitEmail string) []byte {
	bundle := "0"
	if meta.Bundle {
		bundle = "1"
	}
	return []byte(strings.NewReplacer("@ID@", id, "@BRANCH@", meta.Branch, "@START@", meta.Start,
		"@BUNDLE@", bundle, "@GIT_NAME@", gitName, "@GIT_EMAIL@", gitEmail, "@DEPS@", jobproto.DepsFunc).Replace(jobScript))
}

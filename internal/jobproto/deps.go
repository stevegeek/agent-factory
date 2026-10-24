package jobproto

// DepsFunc is the shell function deps that a job's scripts share (a pull
// host's job.sh and gate.sh; a helper's job script with [remote] deps):
// the checkout's dependencies, installed from the network into the job's
// caches (the image's BUNDLE_PATH; a shard's lib/) when they are not there
// yet, as $JOB/deps.log records. A Lever worker and a deps helper have
// egress and their own caches; the dispatcher's agents have a pre-seeded cache
// and no network instead, which is why a project's install command only
// checks. It runs in the checkout, as the job's user, and fails when an
// install fails.
const DepsFunc = `deps() {
  if [ -f Gemfile ] && command -v bundle > /dev/null 2>&1; then
    bundle check > /dev/null 2>&1 || bundle install >> "$JOB/deps.log" 2>&1 || return 1
  fi
  if [ -f shard.yml ] && command -v shards > /dev/null 2>&1; then
    shards check > /dev/null 2>&1 || shards install >> "$JOB/deps.log" 2>&1 || return 1
  fi
  return 0
}`

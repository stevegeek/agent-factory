
set -u
cd /job
if [ -f repo.bundle ]; then
  git -c init.defaultBranch=main clone -q --no-checkout repo.bundle repo && cd repo \
    && git checkout -q -B "$JOB_BRANCH" "origin/$JOB_START" || exit 1
else
  # a task with no project: scratch space, a repository only so the head check works
  mkdir repo && cd repo && git init -q -b "$JOB_BRANCH" \
    && git -c user.name=job -c user.email=job@localhost commit -q --allow-empty -m scratch || exit 1
fi
cp -a /job/untracked/. . 2>/dev/null || true
pi --provider anthropic --model "$JOB_MODEL" -p --no-session --no-approve \
   --append-system-prompt /etc/agent-rules.md --append-system-prompt /job/role.md \
   < /job/prompt.md > /job/agent.log 2>&1
echo "pi exited $?" >> /job/agent.log
git rev-parse "refs/heads/$JOB_BRANCH" > /job/head
if [ "$JOB_BUNDLE" = 1 ]; then git bundle create -q /job/out.bundle "refs/heads/$JOB_BRANCH"; fi
if [ -d /job/memory ]; then tar -C /job/memory -cf /job/memory.tar .; fi

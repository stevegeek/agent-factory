# agent-factory

Orchestration for unattended coding agents on small, resource-constrained machines. A file-based board drives each ticket through fresh, sandboxed agents (triage → work → review → security, and on a public project a public review) and a deterministic gate, then opens a pull request through a GitHub App. Agents never push; the dispatcher pushes only `bot/*` branches, and merging stays with a person.

Agents: Claude Code via [Scion](https://github.com/GoogleCloudPlatform/scion) (local mode, rootless Podman), or [Pi](https://www.npmjs.com/package/@earendil-works/pi-coding-agent) for hosts whose CPU can't run Claude Code's native binary.

## Layout

| Path | |
| --- | --- |
| `cmd/factory` | the dispatcher (Go, standard library only): board, pipeline, gate, doors, `factory render` |
| `cmd/foreman` | Foreman, the maintainer's web app: desk of asks, weekly floor, boards, the ticket API and MCP |
| `cmd/factory-remote` | on a helper host: runs dispatcher stages as Pi or Claude Code jobs (the dispatcher's SSH key is forced to it); on the dispatcher, `factory-remote mailbox HOST` is a pull host's forced command |
| `cmd/factory-pull` | on a pull host (the desktop): claims its jobs from the dispatcher's mailbox and runs them in Lever workers |
| `internal/` | the packages behind them; `internal/archtest` holds the import table that keeps Foreman away from GitHub, git writes and agents |
| `build.sh`, `build/` | the one build entry point: the pinned Go toolchain (`build/go.sha256`), the build's memory limits |
| `bin/pi-run` | runs Pi in a capped container |
| `deploy.sh` | build, render and install on a host over SSH |
| `tools/capture-live.sh` | a private snapshot of the live board for the live-board tests |
| `host/setup.sh` | host base setup: zram, systemd-oomd, locked `factory` user, memory budget, rootless Podman |
| `host/slim.sh` | optional: strip the Ubuntu server install to what the factory uses (dry run by default) |
| `host/macmini.sh` | optional, on a Mac mini (6,1 or 8,1): reboot on panic, hardware watchdog, no sleep, radios off, Wake-on-LAN, power on after AC loss |
| `host/allow-dispatcher.sh` | on a helper host: authorise the dispatcher's key, forced to `factory-remote` |
| `images/claude/` | Scion/Claude Code agent image + rules every agent gets |
| `images/pi/` | Pi agent image |
| `images/lever/` | the instructions of a Lever worker that takes factory jobs |
| `templates/<flavour>/` | Scion role templates (`ruby-gem` ships) |
| `systemd/` | tick / sweep / clock / rebuild timers, Foreman's user unit, its wake and screen-relay path units, the backup timers |
| `examples/config/` | starting config |
| `examples/mcp/` | an agent's MCP config for Foreman's `/mcp` |
| `examples/lever/` | a pull host's Lever instance (`lever.yaml`), `factory-pull`'s config and user unit |
| `skills/tickets/` | the skill that tells an agent how to work from the board |

## Setup

Host (Ubuntu 26.04), once, as admin:

```sh
sudo ADMIN_USER=you bash host/setup.sh
# 2 GiB host: add BUDGET_MEMORY_HIGH=1000M BUDGET_MEMORY_MAX=1200M BUDGET_SWAP_MAX=768M
bash host/slim.sh                  # optional: see what slimming would remove (snapd, cloud-init, kdump, ...)
sudo bash host/slim.sh --apply     # then reboot; --undo reinstalls
```

Config repo (yours, private): `config.toml` (copy `examples/config/config.toml`), optional `rules.md` appended to the agent rules, this repo as a submodule:

```sh
git submodule add https://github.com/stevegeek/agent-factory core
core/deploy.sh --config . --host factory@host --install-go    # the first time: puts the pinned Go toolchain on the host
core/deploy.sh --config . --host factory@host                 # after that
```

See [Deploy](#deploy) for what it builds, installs and keeps.

On the host, as `factory`:

```sh
# Claude Code: build sciontool + scion, then the image
podman build -t localhost/factory-agent:scion ~/factory/images/scion-agent   # needs ./sciontool in that dir
factory patch-scion ~/path/to/scion/harness/provision.py   # after each Scion install or upgrade (local-mode trust path)
claude setup-token        # save the printed token to ~/.config/factory/claude-oauth-token, mode 600
# Pi: Anthropic API key (subscription OAuth is for Anthropic's own apps only)
podman build -t localhost/factory-pi:dev ~/factory/images/pi
umask 077; cat > ~/.config/factory/anthropic-api-key
```

Timers, as admin: `sudo bash ~factory/factory/systemd/install-units.sh --enable`

### Helper hosts

A second host can run stages as jobs, Pi or Claude Code: as overflow for the stages it lists when the dispatcher's own slots are full, or for any ticket pinned to it (`--host`). On the dispatcher, as `factory`:

```sh
ssh-keygen -t ed25519 -N '' -C dispatcher -f ~/.ssh/factory-remote
ssh-keyscan HELPER_IP >> ~/.ssh/known_hosts      # compare with the helper's host key
```

A helper gets its files from the dispatcher's deploy, relayed through the deploying machine (the dispatcher's own key on the helper only runs `factory-remote`): `core/deploy.sh --config . --host factory@host --helper factory@helper`. It needs no Go toolchain: it gets the static `factory-remote`, with `bin/factory` a link to it that runs only `rebuild` and `version`, plus `config.toml`, the rebuild and watchdog units (armed only by `install-units.sh --helper --watchdog`), `images/pi` and the rendered `images/scion-agent` (without `sciontool`).

On the helper, after `setup.sh`, a deploy, the Pi image build and its API key, as admin:

```sh
sudo bash host/allow-dispatcher.sh --from DISPATCHER_IP --key "$(cat factory-remote.pub)"
```

Then `[hosts.<name>]` in the config (see `examples/config/config.toml`). Jobs get the same `report.json` contract; a worker's commits come back as a git bundle, and a reviewer's job reports the head it left so a moved branch is caught. The helper's `[remote]` table, not the dispatcher, decides agent, credential, image, memory, cpus, mounts, env and max_jobs.

Every helper gets the same `config.toml`, so one helper's own settings go in `[remote.<name>]`, over `[remote]`: `<name>` is the helper's short hostname (`hostname -s`, lowercased), which only root on the helper sets, so the dispatcher cannot pick another host's table. No helper is named after a `[remote]` key (`env`, `image`, `agent`, ...); `factory check` refuses such a table, and one whose name is not lowercase, which never applies. Its `env` adds to `[remote] env`; its other keys replace. `factory check` on the dispatcher checks every table.

- `agent = "pi"` (the default): Pi in the Pi image, with the API key.
- `agent = "claude"`: Claude Code (`claude -p --dangerously-skip-permissions`, as Scion's Claude harness runs it: the container is the sandbox) in the image `image` names, which must be set (the Pi image is refused), e.g. `localhost/factory-agent:scion` with its entrypoint replaced; the image's managed rules load by themselves, the role is appended to the system prompt and the prompt comes on stdin. It needs SSE4.2+.
- `auth`: `"subscription"` (the default for claude) reads `~/.config/factory/claude-oauth-token` (`claude setup-token`, mode 600) into `CLAUDE_CODE_OAUTH_TOKEN`; `"api-key"` (Pi's only kind) reads `~/.config/factory/anthropic-api-key` into `ANTHROPIC_API_KEY`. A job gets that one credential, through podman's environment and never argv; `[remote] env` cannot set either, and a missing or empty file fails the job before it starts. The job can read it, so its content (a prompt, an issue's text) can spend it or send it away: `"subscription"` exposes the maintainer's long-lived setup token as the dispatcher's own Scion agents already do (revoke it in claude.ai's settings, make a new one with `claude setup-token`). A helper that takes untrusted input is better on `"api-key"` with its own key and a spend limit.
- `cpus` (2) is the job's `--cpus`.
- `deps = true` (default false) installs the checkout's dependencies before the agent starts, after the pinned files are copied in: `bundle check || bundle install` and `shards check || shards install`, the same function a pull host's `job.sh` runs, logged to the job's `deps.log`. It runs without the job's credential in its environment (a Gemfile and a gem's extconf are code from the checkout), and, when the image has `flock` and `BUNDLE_PATH` is a writable directory, one job at a time on the shared cache (`.factory-deps.lock` there; a wait over 900 s is a failed install). So a helper can take every stage, work on gems included, without a hand-seeded cache. If an install fails the agent does not run: the end of `deps.log` goes to `agent.log` and the job reports `blocked` with that reason, so the ticket reaches a person saying why rather than as "finished without a report". The cache must be mounted writable for that host: `mounts = ["{root}/cache/bundle:/home/scion/.bundle-cache"]` (no `:ro`) with `BUNDLE_PATH` pointing there. A job runs as uid 1000 with `--userns=keep-id:uid=1000,gid=1000`, which maps the factory user to uid 1000 inside, so a cache directory the factory user owns (`mkdir -p ~/factory/cache/bundle` as `factory`) is writable with no chown; one owned by root or a subuid is not. Accepted risk: a shared writable cache lets one job plant gems that later jobs on that helper load, as with the desktop's Lever worker caches; gates never run on an ssh helper, so a planted gem cannot reach the gate.

```toml
[remote.helperb]
agent = "claude"
image = "localhost/factory-agent:scion"
memory = "3g"
cpus = 4
max_jobs = 2
```

On a Claude Code helper, as `factory`: copy `sciontool` into `~/factory/images/scion-agent/` (the deploy leaves it out), `podman build -t localhost/factory-agent:scion ~/factory/images/scion-agent`, and save the token (or the API key with `auth = "api-key"`). Its weekly `factory rebuild` rebuilds that image, the claude kind, and no Pi image. Every tick also asks each helper for its vitals (the `vitals` verb: load, memory, disks, temperature, running containers and the versions in its `images.json`, as JSON), all hosts at once and each within 7 s, for Foreman's System card.

### Desktop worker (Lever)

A host the dispatcher cannot reach, such as a workstation on the far side of a one-way firewall, can still take jobs: it pulls them. Its `[hosts.<name>]` says `transport = "pull"`. Its jobs wait in a mailbox on the dispatcher, `~/factory/mailbox/<host>/<job>/`, and the host calls in over SSH with a key forced to `factory-remote mailbox <host>`, which serves six verbs for that host only: `claim` (the oldest queued job's tar, or exit 3), `beat` (a log tail; answers `ok` or `cancel`), `done` (the collect tar, checked and capped), `release`, `giveback` (a claimed job no worker started, with why) and `vitals` (the host's vitals as JSON, at most 64 KiB, checked and stamped with the dispatcher's clock into `mailbox/<host>/vitals.json`, which the tick shows on Foreman's System card). On the host, `factory-pull` (a user service) asks the instance's broker for its workers every step (`GET /operator/worker/list` on `.lever-state/operator.sock`), starts any configured worker that is stopped or suspended (`POST /operator/worker/start`, at most once every 2 minutes each), and claims a job only when a [Lever](https://github.com/stevegeek/lever) worker is running and idle at its prompt (`idle_at_prompt`) with nothing unfinished in its `jobs/`. While the broker does not answer it claims nothing, and it logs Lever's state once per change. It unpacks the job into that worker's `workers/<fwK>/jobs/<id>/` with the helper's hardened extract and caps. It writes the job's `job.sh` (the git steps, from its own template) and sends the worker an operator note, `job <id> ready at jobs/<id>`. The worker must show it took the job within 3 minutes (Lever reports it busy, or it makes `repo/`, `progress.log`, `gate.out`, `head` or `report.json`); if the note fails, or the worker does not take it in time (it is then interrupted), the job is given back with why. It beats every minute with the worker's `progress.log`, and when `report.json` appears it posts `report.json`, `prompt.md`, `head`, `out.bundle`, the log and `memory.tar`: everything `factory-remote collect` sends except `usage.json`. The worker, Claude Code in Lever's jail, verifies the note with `message_verify`, runs `job.sh prepare`, does `prompt.md` with `role.md`, runs `job.sh finish` and writes `report.json` last. `factory-pull` never runs git or anything else from a job's directory. It reads and writes there only through an `os.Root`, so a link a worker plants leads nowhere. Every `vitals_seconds` (60; 0 turns it off) it posts the desktop's vitals: load, memory, temperature, the disks of `/` and the home, and its workers (`{"up", "running", "free", "note"}`: whether the broker answered, how many configured workers run, how many could take a job now, and which are not running). Set `autostart: true` on the workers in the instance's `lever.yaml` too, so `lever up` starts them without waiting for `factory-pull`.

On the dispatcher the job is a helper job like any other: the same `report.json` contract, the bundle fetched, the head checked, review and Gate 1 as usual; GitHub pushes stay on the dispatcher. A queued job is waited on like a running one. An unclaimed job past `claim_minutes` (30), or a claimed one with no beat for `beat_minutes` (10), is missing: the agent is counted lost, as for a helper job that disappeared. A pull host that has not called in for `beat_minutes`, or whose last vitals (no older than `beat_minutes`) say its broker is down or no worker runs, is away: its overflow goes elsewhere while a ticket pinned to it waits, and a job still queued for it is taken back and placed again at once, not after `claim_minutes`. While it reports fewer workers running than its `slots`, it gets no more jobs than that. A given-back job is removed and its ticket goes straight back to its stage's column for another host (an `agent_lost` move saying why), and the host gets no new job for 10 minutes; a given-back gate job is lost, so the gate runs again. A `factory-pull` from before `giveback` releases such a job instead, and one from before the workers in its vitals is judged by its calls alone. Removing a job (its timeout, or `factory move`) reaches the host as `cancel`: the worker is interrupted and the claim released. On the host, a job past its `timeout_minutes` is interrupted and posted with an error report. The job's model is the Lever worker's, not `[models]`.

**Gate jobs.** A pull host can run Gate 1's checks too, so a heavy gate (a Crystal compile) does not need the dispatcher to itself. With `"gate"` in its `prefer` it takes every gate; with `"gate"` in its `stages` it takes only a gate that would wait on the dispatcher (one whose `gate_memory` is above `[gate] heavy_memory`, or any while every slot there is taken). A host that is away, paused, full or refusing leaves the gate on the dispatcher as before; only a pull host runs gate jobs (`factory check` warns about `"gate"` on an ssh host). Only the checks move. The dispatcher takes the branch's and the base's heads and records them on the ticket, and sends a job of kind `gate`: `meta.json` (the gated commit as its start, 35 minutes), `checks.sh` (the project's `set -e; install; test; lint`), a bundle of the base and the branch, and the pinned untracked files. `factory-pull` writes `gate.sh` from its own template instead of `job.sh`, and the worker only starts it: a fresh clone of the bundle with the gated commit checked out detached, the pinned files copied in, `checks.sh` with its output in `agent.log` and a time limit, then `report.json` with `{"status": "done", "gate_exit": N}` (or `"status": "error"` when the checks could not run). When it is collected, the dispatcher decides from the heads it recorded: the changed files and size, the blockers, the move (its detail ends `(checks on <host>)`, and the gate record has `host`), the PR update and the audit, as for a gate run here; the output goes to `logs/<id>/gate-output.txt`. A gate job that is lost, times out or ran no checks is never a failed gate: the ticket stays in the gate column and is gated again, on the dispatcher after two lost jobs. Two differences from a gate here: the checks run in the worker's environment (its toolchain and caches, Lever's egress, no podman memory limit) rather than the gate image with no network, and their result is reported by a machine the dispatcher does not control. It only ever sets `checks_pass`; merging still needs the maintainer's approval on GitHub, and the merge queue's re-check on the current base (`still_holds_on_main`) and the image rebuild's checks always run on the dispatcher.

On the dispatcher, allow the desktop's public key from its address. On a host set up by `host/setup.sh`, as root (after a deploy, so `factory-remote` exists):

```
sudo bash host/allow-puller.sh --host desktop --from DESKTOP_IP --key 'ssh-ed25519 AAAA... factory-pull'
```

It keeps one line per pull host in the root-owned `/etc/ssh/authorized_keys/factory.pullers`, forced to `factory-remote mailbox desktop` from that address only. `setup.sh` lists that file in sshd's `AuthorizedKeysFile` and never writes it, so a re-run of `setup.sh` keeps the key (an older `setup.sh` did not list it: re-run the current one first, which the script checks). Re-running for the same host replaces its line, and `--undo --host desktop` removes it. On a host without `setup.sh`, add the same line to the factory user's `~/.ssh/authorized_keys` by hand:

```
restrict,from="DESKTOP_IP",command="/home/factory/factory/bin/factory-remote mailbox desktop" ssh-ed25519 AAAA... factory-pull
```

and the host to `config.toml`, then deploy as usual:

```toml
[hosts.desktop]
transport = "pull"
slots = 3                        # the number of Lever workers
stages = ["triage", "review"]    # overflow when the dispatcher is full
prefer = ["task", "work"]        # taken here first while the desktop is up
claim_minutes = 30
beat_minutes = 10
notes = "the desktop: Claude Code in Lever, up some hours most days"
```

On the desktop, as yourself:

1. Build `factory-pull` (standard library only): `cd core && CGO_ENABLED=0 go build -trimpath -o ~/.local/bin/factory-pull ./cmd/factory-pull`.
2. A key and the dispatcher's host key: `ssh-keygen -t ed25519 -N '' -C factory-pull -f ~/.ssh/factory-pull`, then `ssh-keyscan DISPATCHER_IP >> ~/.ssh/known_hosts` (compare it with the dispatcher's host key). Add a `Host factory-master` entry to `~/.ssh/config` with `HostName DISPATCHER_IP`, `User factory`, `IdentityFile ~/.ssh/factory-pull` and `IdentitiesOnly yes`. The alias names the master's role, not its machine, so a move of the master changes only its `HostName`; `factory-pull`'s `ssh` setting names the alias.
3. The Lever instance, for example in `~/lever/factory-worker/`:
   - Copy `examples/lever/lever.yaml` and `manager.md` there, and set the scion pin and the sizes.
   - Make the tree with `mkdir -p tree/workers/fw1 tree/workers/fw2 tree/workers/fw3`.
   - Build the workers' instructions from the protocol and the factory's rendered agent rules: `cat core/images/lever/factory-worker.md RENDERED/images/scion-agent/agent-rules.md > factory-worker.md`. `RENDERED` is the output of `factory render CONFIG_DIR RENDERED`, or copy `~factory/factory/images/scion-agent/agent-rules.md` from the dispatcher.
   - Save `claude setup-token`'s token to `secrets/claude-oauth-token` (mode 600).
   - Build the image `lever.yaml` names, as Lever's docs describe, then run `lever up --no-attach` (or use Lever's oneshot user unit).
   - Once, through `lever attach`, ask the manager to start fw1, fw2 and fw3.
4. `~/.config/factory-pull/config.toml` from `examples/lever/factory-pull.toml`. Use the dispatcher's `[identity]` `git_name` and `git_email`, so the commits carry the factory's identity.
5. The service: `install -Dm644 core/examples/lever/factory-pull.service ~/.config/systemd/user/factory-pull.service`, then `systemctl --user daemon-reload && systemctl --user enable --now factory-pull`. Run `factory-pull -once` for a single pass by hand, and `journalctl --user -u factory-pull` for its log. `systemctl --user stop factory-pull` takes the desktop out; the dispatcher stops sending it work after `beat_minutes`.

## Deploy

`deploy.sh` runs on your machine and talks to the host over SSH as the factory user:

```sh
core/deploy.sh --config DIR --host SSH_HOST [--build host|local|helper] [--verify] [--helper SSH_HOST]...
               [--install-go] [--commit REV] [--prebuilt DIR] [--allow-older] [--dry-run]
```

Before anything changes on the host, deploy refuses a rollback: a core commit that does not contain the core the host runs (the commit line of its `bin/BUILD`), a config checkout whose `HEAD` does not contain the config commit it runs (`deploys/config-commit`), and a `--config` that is not a git checkout while the host records a config commit. A lagging `core/` submodule (`git submodule update --remote core`) or an unpulled config is the usual cause; `--allow-older` installs it anyway (a deploy from a plain directory then leaves the host with no config commit recorded). The master's self-deploy checks the core the same way.

1. **Source**: an archive of the commit (default `HEAD`; uncommitted changes never ship) into `~/factory/src/<commit>/` (mode 0700, never mounted into an agent). The last two trees are kept.
2. **Build**: `build.sh` builds `factory`, `foreman` and `factory-remote` with the pinned official Go toolchain, never a system `go`. `--build host` (the default) builds on the host, inside a memory-capped user scope and only while no agent runs (it waits up to two hours); `--build local` builds on your machine with the same toolchain and copies the binaries across; `--verify` builds in both places and installs only if the two `BUILD` files (sha256 of each binary) are identical. The build is reproducible: the same commit gives the same bytes anywhere. `--install-go` downloads the pinned tarball on your machine, checks it against `build/go.sha256` and installs it on the host (`FACTORY_GO_TARBALL` names one already downloaded). `--prebuilt DIR` installs binaries built elsewhere, checked against their `BUILD`.
3. **Smoke test** of the new binaries on the host: `factory render` of your config, `factory check` against it, `factory check --board --asks` against the live root (read-only: every ticket file reads cleanly and every ticket in an ask column has its stored ask), `factory model`, `foreman --help`, `factory-remote version`. Any failure installs nothing.
4. **Install**, holding `.tick.lock` so no tick runs meanwhile: `bin/factory`, `bin/foreman`, `bin/factory-remote` (each by an atomic rename, so a running binary keeps its file; the previous Go binary stays as `bin/<name>.prev`, for a swap back by hand), `bin/BUILD`, `bin/pi-run`; `config.toml` (the previous one as `config.toml.prev`), `context/` (replaced whole), `systemd/`, `images/`, the Scion templates; Foreman's user unit, sandboxed when the host's user manager can apply it, with Foreman restarted if it is enabled; `factory-wake.path` and `factory-screens.path` (and their services), enabled. The first Go deploy removes the Python files deploys used to ship and moves Foreman's state into `~/factory/foreman-state/`.
5. **Helpers** (`--helper`): `factory-remote` (its sha256 checked against `BUILD`) and the helper's files, relayed from the host through your machine.

When the host's user manager cannot apply Foreman's whole sandbox, deploy installs the unit with as much of it as applies (the `InaccessiblePaths` lines, with `PrivateUsers` if that applies), with a warning; with none only if not even those apply.

The board, memory, logs, token store, `images.json`, the backup key and the secrets are never touched. `--dry-run` deploys into a new temporary directory on your machine instead of a host and lists what it installed; a host written `local:DIR` does the same into `DIR`. Neither runs `ssh`, `systemctl` or `systemd-run`: deploy prints what it would run.

### Builds on a helper

`--build helper` builds on a builder helper instead of the host or your machine: after the source is on the host, the host runs `factory build <commit>`, which sends that tree to the helper named by `[build] host` and stores the checked result as a candidate in `~/factory/builds/<commit>/` (the newest five are kept); deploy then installs the candidate. A commit that already has a candidate is not built again, and one the host already runs is reused as always. `[build] host = "self"` has a strong master build its own candidates: `factory build` runs its own `bin/factory-remote build <commit>` (no ssh, the same container, timeout and checks), which reads this host's `[remote.<hostname>]` (`build = true`, `image`, `cpus`, `build_memory`); `factory check` wants that `build = true`, and no `[hosts]` entry.

On the helper, `factory-remote build <commit>` runs the tree's `build.sh` with the pinned Go in a container with no network and no credential: the source and the toolchain mounted read-only, a fresh build cache, `--cap-drop=all`, one build at a time. Only the binaries come back; the host checks each sha256 against `BUILD` and that `BUILD` names the commit and the pinned Go before it stores them. Set it up once:

1. In config: `[build] host = "<a [hosts] name>"` (an ssh helper) and, in that helper's own table, `[remote.<its hostname>] build = true` (`build_memory`, default `2g`, is the container's memory). Deploy with `--helper` so the helper gets them.
2. On the helper, as `factory`, the pinned toolchain: copy `go<pin>.linux-amd64.tar.gz` (the pin is `go.mod`'s `toolchain` line), check it against `build/go.sha256`, and extract it to `~/.local/share/factory/go/go<pin>` (so that `bin/go` is there).
3. The helper's agent image needs `bash`, `awk` and `sha256sum` (the Claude Code image has them).

A build runs whatever `build.sh` the commit holds, but inside that container: it can only produce binaries, which run only once you deploy them.

### Moving the master

The master role (the tick, Foreman, the board and memory, the secrets and the mailbox, backups and timers, the dispatcher key and a pull host's key) can move to another machine with under an hour of Foreman downtime and no change on any client at the move. What makes that work, set up while the old master still runs:

- **A stable address.** Foreman answers on a name that belongs to the role, not the machine: a Tailscale Service (for example `svc:foreman`, reached as `https://foreman.example.ts.net`), listed first in `[foreman] hosts` (see [`FOREMAN.md`](FOREMAN.md#the-address)). Push subscriptions, the session cookie and the installed app are bound to the origin, so the phone is re-added once, on that name, while the old name still serves. The desktop reaches the master through aliases named for the role too: `factory-master` (the pull key, `factory-pull`'s `ssh`) and `master-factory` (the admin key: deploys and `factory` commands). At the move only their `HostName` changes.
- **One backup line.** `[backup] name = "master"` names the archives `factory-master-*`, so retention and `verify` keep one line across the move instead of starting a new one under the new host name.
- **Backup, restore and adopt move the state.** They are the tested daily path: the backup takes the tick lock and leaves out locks, sessions and credentials, so the move is also a disaster-recovery drill. `factory backup restore` refuses the live root on purpose; `factory backup adopt DIR` moves the restored tree into the new master's fresh root (see Backups under [Upkeep](#upkeep)). The clones and gem cache are large and can be made again, so they go by `tar` over ssh; the secrets (the App key, the Claude token, `foreman-secret`) are copied by hand. Foreman keeps its VAPID key and devices (they are in the archive) and its secret (sessions are HMACs over it), so the phone stays logged in and subscribed.
- **The pull key outside `setup.sh`'s file.** `host/allow-puller.sh` keeps each pull host's key in `/etc/ssh/authorized_keys/<user>.pullers`, which `setup.sh` never rewrites (see [Desktop worker](#desktop-worker-lever)), so a re-run of `setup.sh` on either master keeps it.
- **The master can build for itself.** With `[build] host = "self"` (see [Builds on a helper](#builds-on-a-helper)) a strong master builds its own candidates; its `[remote.<hostname>]` table keeps `build = true`, `image` and `cpus`, since `factory-remote build` falls back to `[remote] image` otherwise.
- **A demoted master's timers cannot tick.** `systemd/install-units.sh --helper` stops and disables the dispatcher units (`factory@.service` and the tick, sweep and clock timers) one by one, then deletes them rather than only disabling them: `host/updates.sh`'s reboot window starts `factory-tick.timer` when it ends, and a unit file that is not there cannot be started. Foreman's wake path is a user unit `deploy.sh` enables, which runs `bin/factory tick` itself, so the cut disables it by hand (below).

The cut, in outline: drain by switching the projects off (`factory project disable`); on the old master stop Foreman, disable its wake and screen paths (`systemctl --user disable --now factory-wake.path factory-screens.path`: the old root keeps its full `bin/factory` for the week below, and a touched `foreman-state/wake` would otherwise tick the stale board), remove the dispatcher units (`install-units.sh --helper`), disable the rebuild and backup timers and check the tick lock is free; take a final `factory backup`; on the new master restore it, `adopt` it, copy the clones and secrets over ssh, deploy, run the root steps (`setup.sh`, `foreman-system.sh`, `allow-puller.sh`, `install-units.sh --enable`), give each helper the new master's dispatcher key with `allow-dispatcher.sh` (it replaces the old line), and move the Service: the old master stops advertising it before the new one does. Check `/healthz` and a test push through the stable address, then switch the projects back on. Addresses stay with the hardware: there is no IP swap. Leave the old root untouched for a week so failback is only re-arming its units (and its wake and screen paths); after that, rename it and demote the machine to a helper.

The old master never takes over by itself. A watchdog on another host may only notify: from outside, a dead master and a network split look the same, and a takeover risks two masters on two boards. Failback is a short manual runbook.

### Self-deploy

With `[deploy]` in config (`core` and `config`, each `owner/repo`; `config_dir`, default `config`, the config repo's directory that holds `config.toml`), the master deploys itself: core `main` with config `main`, built on the builder helper. `[github] installation_id` and `[build] host` must be set too (`factory check` says so).

- **What starts one.** `factory deploy start`, the maintainer's "Deploy it" on a Deploy? ask, and a merge. The tick never deploys: a start writes `~/factory/deploys/request.json` and starts the transient user unit `factory-deploy` (3 h at most), which runs `factory deploy run`. A start while one runs leaves its request for the running one, which looks for one once more as it finishes; with no user bus the request waits, and `factory deploy run` runs it by hand.
- **Always `main`.** A deploy takes core `main`'s head and config `main`'s head when it runs, whatever rev a ticket or ask shows, so an old ask cannot roll the master back. Only `factory deploy start REV` names an older commit, and it must be on core `main`; anything else goes from the desktop. Core is fetched with no credential (it is public) into `deploys/core.git`, config with a token the App mints for that one repo, `contents: read`, into `deploys/config.git` (the App's installation must include the config repo).
- **Config `main` is trusted.** Config `main` now decides what the master runs (its `config.toml`, and through `[deploy]` the repos the next deploy fetches and runs `deploy.sh` from), so only the maintainer pushes to it. A self-deploy refuses a config commit that changes `[deploy]` `core`, `config` or `config_dir` (nothing installed, with a notice): changing those takes a desktop deploy. Once the config repo is in the App's installation, the App's tokens reach it as they reach the projects, and only `Writer.Push`'s `bot/*` rule stops one of them writing to its `main`; protect that branch on GitHub as well.
- **The hold.** `factory deploy hold [--note TEXT]` (or Hold on System status) turns a merge's deploy into one Deploy? ask on the desk, which a newer merge updates instead of filing a second; `factory deploy unhold` releases it. `factory deploy ask` files that ask by hand. A start by hand ignores the hold.
- **The run.** The runner holds `deploys/.lock` (a desktop `deploy.sh` refuses to start meanwhile, and the runner refuses while a `~/factory/.deploy` younger than 2 h says a desktop deploy runs or died). It snapshots the running files into `deploys/before/`, then runs the new commit's own `deploy.sh --host self --build helper`, staged in `deploys/work/` (its output in `deploys/logs/`, the newest 10 kept). The check: within 90 s Foreman's `/healthz` says `ok` and `version <commit>` (the old Foreman says ok until it restarts, so the version is what counts; for a deploy of the running commit, config only, its `started <time>` line must also be after `deploy.sh` began, and after a restore that put back `foreman.env`, after the restore), then `bin/factory version` names the commit. A good deploy records the config commit in `deploys/config-commit` (as `deploy.sh` does from a config checkout) and then sends each ssh helper the new `factory-remote` (below).
- **The rollback.** A failed check, or a `deploy.sh` that failed after it changed something, puts back from the snapshot each of `bin/factory`, `bin/foreman`, `bin/factory-remote`, `bin/BUILD`, `bin/pi-run`, `config.toml` and `foreman.env` whose bytes differ (each binary first checked against the snapshot's `BUILD`), restarts a user-unit Foreman (a system unit restarts on its own), and checks again against the old commit. `context/`, the rendered units, images and templates stay new. A notice says which: "failed: nothing installed" (with the log's end), "failed its check: <old> is back", or "the restored build is not healthy: deploy from the desktop". `bin/*.prev` is not used: after a deploy that changed only config it holds an older build.
- **Commands.** `factory deploy status` prints the hold, the running or last deploy (commit, config commit, result, the commit it replaced, each helper's outcome, the log's end on a failure) and a request waiting. `factory deploy restore` puts the snapshot back by hand, as after an interrupted deploy; it is refused while a deploy runs or a young `~/factory/.deploy` is there (a reboot mid-deploy is marked `interrupted` with a notice at the next tick, which offers the restore only when the deploy got past its snapshot; nothing is restored by itself). `deploys/before/SNAPSHOT` names the deploy the snapshot is for, and the restore is refused, unless `--force`, when it would not undo the last deploy: that deploy stopped before its own snapshot, or `bin/BUILD` no longer names its commit (a rollback, a desktop deploy) and it did not end `interrupted` or `rollback_failed`. `factory deploy start REV --drill` fails the check on purpose to try the rollback; it is refused on the running commit, where there is nothing to roll back. A start of a commit older than the running one is refused by `deploy.sh`, nothing installed, unless `--allow-older` (a drill allows it: it restores what it installed).
- **Root steps.** A deploy that changed `host/`, `systemd/factory@.service`, a `systemd/*.timer`, `systemd/factory-rebuild.service`, `systemd/factory-watchdog.service` or `systemd/install-units.sh` since the commit it replaced files "Deployed <commit>: root steps wait for you" with the list (or "unknown base" when that commit is not on core `main`). The root steps stay yours.
- **Helpers.** After a good check each ssh helper gets `factory-remote install <commit>`: the bundle `--helper` relays (`factory-remote`, `BUILD`, `pi-run`, `config.toml`, the units and image contexts, without `sciontool`). A helper takes it only if you have created `~/factory/accept-installs` there (nothing writes it); it checks the binary against `BUILD`, runs the new one, and installs with `.prev` and `config.toml.prev`. **This is trust in the master:** an opted-in helper runs the code and the `config.toml` (its `[remote]` settings included) the master sends; the sha256s only catch damage on the way. Each outcome shows on System status; a helper that was off gets no notice and is tried again at the next deploy.
- **The desktop.** `deploy.sh` from the desktop stays the bootstrap (the first deploy of `[deploy]` and of `install`, a new helper, a pull host's `factory-pull`, which the master cannot reach) and the fallback when a self-deploy cannot get the master healthy.

### Operator

The manager (a `manage` ticket on project `factory`, from Foreman's ＋ "Ask the manager" or its Conversation) is also the factory's operator. It reads the board, the logs, `config.toml`, System status (`sysinfo.json`, with the project switches and the deploy hold), the live `context/` and `deploys/`, all read-only, and answers. Its report may also propose **drafts** (unified diffs against the config repo: `config/config.toml`, `config/rules.md`, `config/context/…`) and typed **actions**. It never applies anything: on each pass, after the deploy drain, the tick (`internal/operator`) judges every new or tapped item from the change itself, never from the manager's claim, and records the tier, the places, the walls and the result on the item.

| Tier | Config places | Actions |
| --- | --- | --- |
| Applies by itself | a schedule's `every_days` set to at least 7 or to no less than before; projects removed from a schedule's `projects` (present in both); `projects.<k>.clock = false`; `projects.<k>.enabled = false` | `pause_host` (1–168 h, default 24), `resume_host`, `pause_project`, `deploy` (main) |
| Needs your tap | everything else, including any unknown place, a diff with no place (comments only), `config/rules.md` and context notes; walls are flagged (`image`, `template_flavour`, `templates.*`, `gate.*`, `triage.*`, `findings.*`, `hygiene.*`, `identity.*`, `github.open_prs`, remotes, `hosts.*.ssh`/`transport`, a project's `walls`, `hygiene`, `self_deploy`, `tier3_paths`, `text_paths`, `mode`, `github`) | `enable_project`, `rebuild` |
| Only you, by hand (refused) | `[github]` `reviewers`, ids and `key`; `[backup]`, `[host]`, `[deploy]`, `[foreman]`, `[board]`, `[build]`; `[[watches]]`; `mounts` and `env` of `[gate]` and any remote; a remote's `build` and `auth`; `hosts.*.key`; a changed project `path`, or a new one other than `<factory_home>/projects/<key>`; `pinned_untracked` entries that are absolute or contain `..`; any file outside `config.toml`, `rules.md` and `context/`; symlinks, mode changes, binary files; a config that does not parse or that adds `factory check` problems | any other kind |

A removed key is never automatic (it widens: no `projects` is every project, no `enabled` is true), an added table is judged key by key, and a draft takes its worst place. Context notes need a tap on purpose: the manager reads every ticket, public issue text included, and agents treat context as your knowledge. The automatic tier applies only on a ticket filed by you through Foreman and with every note yours; on any other (a watch's, or one another Foreman user wrote in) every item waits for your tap.

- **How a draft lands.** In a temp clone of the config project at `origin/main` (`~/factory/operator/work/<ticket>-<run>-<n>`, removed on every exit): `git apply`, classify the old and new files there, commit as `[identity]`, push `bot/operator-<ticket>-<run>-<n>` (forced; the name is fixed per item), open a PR or reuse the open one from an earlier attempt, wait up to 30 s for GitHub's mergeability, and merge at the pushed head's sha (GitHub refuses if it moved). A draft whose reverse already applies to `main` is marked applied ("already on config main"). After your tap the draft is applied and classified again on the current `main`, and one that has become refused is not landed.
- **Then one deploy.** The pass's merges and `deploy` actions give one `Offer` of `main`. `Offer` respects the hold file and also folds into an open Deploy? ask (a merge you held with "merge, don't deploy yet"), so a held change is never deployed behind your back. A failed offer says "merged; deploy not offered: …" on the item and in `factory.log`; deploy then with the `deploy` action or `factory deploy start`.
- **Actions.** Host pauses go to `pauses.json` (a shorter pause never cuts a longer one). `pause_project` and `enable_project` set the project switch (`projects.json`: `off`; `follow` when config has it enabled, else `on`), which overrides config `enabled` until it is back to follow. `rebuild` starts `factory rebuild` as the transient unit `factory-rebuild-now`; ticks wait while it builds. There is no restart action.
- **By hand.** With no config project (`self_deploy = "config"`) or no `[deploy]`, drafts are yours to apply by hand ("I've applied it", as before the operator); actions still work. Drafts saved before the operator count as by hand.
- **The tap.** "Apply it" on the desk (anyone's, recorded as theirs) requests every waiting or failed item at once and wakes the tick. Until the tick has run them the desk shows "Applying", "I've applied it" is refused, and dropping the ticket or sending it back waits for the tick. "I've applied it" beside items still waiting for a tap closes the ticket and names them as not applied. To apply only some, send the ticket back with a note. Nothing retries by itself: a failed item keeps its reason (and "PR #n open; Apply it again reuses it") until you tap again.
- **Undo.** Ask the manager for the reverse draft; it is judged like any other. Every landed change is a PR with GitHub's revert button and a merge commit, named in the ticket's story.

### Cutover from the Python factory

Once, for the first Go deploy to a host that still runs the Python factory (the Go suite green, a build on the dispatcher host within `build/limits`). The Python is no longer in this repository; the deploy removes what earlier deploys installed of it:

1. Stop the tick timer and Foreman. `factory-tick.timer` is a system unit: `sudo systemctl stop factory-tick.timer` (the maintainer; a running tick finishes, and deploy waits on `.tick.lock` anyway). Then, as the factory user, `systemctl --user stop foreman`.
2. Deploy: `core/deploy.sh --config DIR --host SSH_HOST --helper SSH_HOST2` (add `--install-go` the first time). Its smoke test runs the new `factory check`, `check --board` and `check --asks` against the live root and installs nothing on a problem.
3. Start them again: `systemctl --user start foreman`, then `sudo systemctl start factory-tick.timer`.
4. Watch the first ticks (`tail -f ~/factory/logs/factory.log`, and Foreman's own actions in `~/factory/foreman-state/foreman.log`; Foreman's Logs page merges both; `factory ls`) and the desk: an answer, a look, a new ticket, push, and an API and MCP call. Run the pem smoke check: deploy's output says "the App key is out of Foreman's reach" and "the user manager is out of Foreman's reach", and Foreman's `/healthz` says ok.

There is no path back to Python: a problem is fixed forward in Go (the previous Go binaries stay as `bin/<name>.prev`; the backups are the safety net).

## Use

```sh
factory add <project> "Title" --body "what and why"   # human door
factory sweep                                          # github door: open issues → triage (untrusted)
factory clock                                          # clock door: [[schedules]] that are due
factory ls [--all] | show T0001 | move T0001 ready --note "approved"
factory host T0001 helper                              # pin to a host (local, a [hosts] name, or auto)
factory add factory "Summarise <url>" --task           # one agent run, answer comes back to you
factory ask T0003 "Did Steep pass on this?"             # a task about a ticket, on its branch; the answer joins its story
factory result T0009                                   # (project `factory` = no repository; --timeout MIN)
factory add <project> "Idea" --intent --body "..."     # design first: a designer proposes
factory result T0010                                   # read the design + proposed tickets
factory approve T0010 [--only 1,3] | factory redirect T0010 --note "..."
factory add <project> "Bug hunt" --task --findings      # a check whose findings are verified, then filed by you
factory findings T0030                                 # its verified findings: verdicts, evidence, how each files
factory findings T0030 --file 1,3 [--as 3=intent] | --file all | --none
factory findings T0030 --verify                        # a finished task whose findings are prose: verify them
factory tick                                           # one pass; the timer runs it every 5 min
factory early-checks T0001 <head>                      # Gate 1's checks of a head now, kept for its gate (the tick starts it)
factory github                                         # check the GitHub App installation
factory pr T0001                                       # push bot/* + open the PR (draft unless auto-eligible)
pi-run --workdir DIR -p "task"                         # one Pi agent; --model / PI_MODEL to override
factory project disable|enable|follow <project>        # pause or switch on a factory project over config's enabled, or follow config again
factory check                                          # config problems: project modes, [board] people, tokens; project switches
factory kinds [--json]                                 # the project kinds, and what each factory project takes from its kind
factory onboard <project> [--checks] [--json]          # is a project ready to switch on? exit 1 on a failed check
factory usage [--days N] [--by project|stage|ticket|host|model] [--json]   # the tokens the agents spent, largest first
```

### Token usage

Every Claude Code run's tokens are recorded, with no change to how Claude Code is started. Claude Code writes a session transcript under the agent's home (`~/.claude/projects/<dir>/<session>.jsonl`, a subagent's one level below), and each assistant line carries the message's id, model and usage. `internal/usage` reads it: each message id counted once with the largest of each number it was written with (a message is written more than once while it streams, and a resumed session can repeat it), summed per model as input, output, cache write and cache read tokens. The home is the agent's to write, so the read follows no link, reads only regular files, caps the names it lists (10,000 per directory, 50,000 in all), the files, bytes, line length and messages it takes (a capped read is marked `partial`), skips a malformed line or a message id over 256 bytes, clamps each message's numbers to what one message can spend (2,000,000 input, cache write or cache read tokens, 256,000 output) and each model's sum to 2^32. The numbers are still the agent's own transcript: a forged one is bounded, not refused. Pi writes no transcript: a Pi run records nothing.

- **Where it is read.** A local run: when its stage ends, from the agent's home under Scion's project config, after `scion stop` and before `scion delete`; a read that fails is logged and the removal goes on. A helper job: `factory-remote` reads the job's home after the agent exits and leaves `usage.json` in the job's work directory, removing anything the agent wrote there first, and `collect` returns it with the other members (64 KiB at most). A job removed before it was done (past its timeout, gone, or its host failing past the timeout) still counts: `remove` reads the home once the container is gone, before it deletes it, and answers `{"removed": "<job>", "usage": {...}}`. A pull host's job: taken if its `done` tar carries `usage.json`; `factory-pull` does not send one, so desktop runs record nothing yet.
- **Where it is kept.** `logs/<ticket>/<run>/usage.json`, the run's own record, and one line per finished run in `~/factory/usage.jsonl`: `{"at", "ticket", "project", "stage", "run", "host", "models": {"<model>": {"input", "output", "cache_write", "cache_read", "messages"}}}`, appended under `usage.jsonl.lock` in one write and never rewritten. An append after a line cut short (a full disk, a crash) starts a line of its own, so only the torn line is lost. Readers take its last 16 MiB a line at a time and keep only the records of their window (Foreman's 30 days, `--days`), skip a line that does not parse, and count a run recorded twice once. A run read only in part carries `"partial": true`. Nothing goes into the ticket file. A run stopped at a usage limit or its timeout is recorded too. The backup archive holds `usage.jsonl`, and `backup adopt` moves it.
- **What shows it.** `factory usage` totals a window (`--days`, default 7) by project, stage, ticket, host or model, largest first, a total with a run read only in part marked `+` (a lower bound; `partial` counts them in `--json`); Foreman's Floor metrics has a Tokens section, and a ticket's page shows each run's tokens and the ticket's total, marked the same way. The numbers are tokens, never money: the agents run on a subscription.

### Paused projects

A factory project with `enabled = false` in `config.toml` is paused: the tick starts no agent or gate for it, and the clock and the sweep file nothing for it; its running agents finish and its open PRs are still followed. Config's `enabled` is the default. The maintainer can switch a factory project on or off without a config change, from Boards in Foreman or with `factory project disable|enable NAME` (over ssh, as the factory user); the switch overrides `enabled` until "Follow config" (`factory project follow NAME`). Switches are kept in `~/factory/foreman-state/projects.json` (`{"<project>": {"enabled": false, "by": "<person>", "at": "<stamp>"}}`, written atomically under a lock), which the tick, the doors, the rebuild, `factory check` and Foreman all read over `config.toml`; each change is a line in `factory.log` (from the CLI) or `foreman-state/foreman.log` (from Foreman), and wakes the tick. A board project, or a name config does not have, is never switched (`factory check` warns of one in the file). A file that does not read switches nothing: every project follows `config.toml`, and the tick and `factory check` say why.

### Board projects

A project with `mode = "board"` has tickets but no pipeline: people and their own agents do the work, and the dispatcher never starts an agent, gate or schedule for it (`factory add`, `move`, `reply`, `ask`, `pr` and the rest refuse its tickets). Its columns are `inbox / next / doing / waiting / done / dropped`. Every ticket, on either kind of project, has a typed history (`type`, `from`, `to`, `reason`, `by`, … beside the old `detail` sentence).

Agents reach tickets through a token each, which names its projects and what it may do there (`read`, `file`, `talk`, `work`, `plan`). Only the token's sha256 is kept, in `~/.config/factory/api-tokens.json` (mode 0600; `FACTORY_API_TOKENS` overrides the path):

```sh
factory tokens add example-agent --project notes_app --can read,file,talk,work   # prints the token once
factory tokens list | revoke example-agent
factory ticket list [--project P] | get T0042 | answer T0042 1 "en-GB"        # the maintainer's view, for now
echo '{"id": "T0042"}' | factory ticket get --as example-agent --json          # what Foreman's API does for that token
```

`factory ticket VERB --json` (create, get, list, mine, note, move, claim, update, ask, answer, split, send, projects, whoami, reindex) takes one JSON object on stdin and answers one on stdout: exit 0 with `{"ok": true, …}`, or exit 1 with `{"ok": false, "error": {"code", "message"}}`. `"format": "md"` makes `get`, `list` and `mine` answer Markdown instead, and a write answer Markdown (after `note`, the conversation's last 5 lines) beside a slim `ticket` (`id`, `project`, `mode`, `column`, `column_label`, `assignee`): what MCP shows an agent. Every decision is made there: what the token sees (a ticket outside its projects is `not_found`), what it may do, the column rules, limits and idempotency keys. No verb approves, merges, opens a PR or changes config; on a factory project a token can only file a change into triage (door `agent`, untrusted) and comment. `board/index.json` beside the tickets is a cache, rebuilt from the files as they change (`factory ticket reindex`).

### Importing from Fizzy

A Fizzy board becomes a board project's tickets in two steps. The export runs wherever the `fizzy` CLI is logged in. It runs the CLI (with `--quiet`, argv only) and never handles a token. The import runs on the dispatcher:

```sh
factory fizzy export --out ~/fizzy-dump [--board BOARD_ID]... [--fizzy-dir DIR]   # DIR: where the CLI's .fizzy.yaml is
#   boards.json; <board>/columns.json, cards.json, cards/<n>.json (with steps), comments/<n>.json; dirs 0700, files 0600
factory fizzy import ~/fizzy-dump --map BOARD_ID=PROJECT [--map ...] --dry-run    # per board: counts by column, the labels
factory fizzy import ~/fizzy-dump --map BOARD_ID=PROJECT [--map ...]              # created, refreshed, unchanged, skipped
```

Every mapped project must already be in `config.toml` with `mode = "board"`. Boards you don't map are listed and skipped.

Each card becomes one ticket, filed through the `import` door, which is untrusted like `agent`. The ticket gets:

- The card's title, and a body made of the description (as Markdown), the steps as a `- [ ]` / `- [x]` checklist, and a line `Imported from Fizzy card #N: URL` (plus `Assigned in Fizzy: …` if the card had assignees).
- The tags as labels, plus `golden` for a golden card.
- An `origin` link to the card.
- The card's creation time, and its creator as the author, written `Name (Fizzy)`. That name is never a person or a token, so it grants nothing.
- The card's comments as conversation comments, oldest first, each with its own author and time.

Fizzy columns map to board columns as follows:

| Fizzy | Board column |
|---|---|
| no column, `Maybe?` | Inbox |
| `Next`, and any other column | Next |
| `In progress`, `Doing` | Doing |
| `Review` | Doing, labelled `review` |
| a column starting `Waiting` or `Blocked` | Waiting, with the column's name as the reason |
| Not Now (postponed) | Waiting, "not now (postponed in Fizzy)" |
| closed, `Done` | Done |

The ticket is filed into Inbox. If its column is another one, the maintainer moves it there with a typed `moved` event, reason `imported`.

Each ticket keeps an `imported` record: the card's id and number, a digest of what the import wrote, the labels it wrote, and the ids of the comments it copied. A rerun finds the ticket by that record, so it never files a card twice. On a rerun:

- If the card changed in Fizzy, the title, body, column and imported labels are refreshed. Labels added on the board are kept.
- Only comments not copied before are added.
- A ticket whose card hasn't changed is left alone, even if it was moved on the board.

Every write goes through the store's create lock and ticket locks. Dump files over 8 MiB, links and odd ids are refused. Control characters are stripped. Titles, bodies, comments and labels are cut to the ticket API's limits.

### Tickets for other projects

Agents in other projects, on any machine on the tailnet, work board tickets through Foreman's MCP server at `https://<host>.ts.net/mcp`, with a token each: `my_work`, `start_work`, `post_update`, `ask_person`, `finish_work`, `file_ticket` and the rest, named for the work. Give an agent a token (`factory tokens add`), the one-line MCP config, and the [`tickets` skill](skills/tickets/SKILL.md) (copy or symlink `skills/tickets` into its `~/.claude/skills/` or the project's `.claude/skills/`; `deploy.sh` doesn't ship it, since these agents run elsewhere):

```sh
claude mcp add --transport http factory https://<host>.ts.net/mcp --header "Authorization: Bearer $FACTORY_TOKEN"
```

Scripts use the same tickets over HTTP at `/api/v1`. Both are in `FOREMAN.md` ([Ticket API](FOREMAN.md#ticket-api), [MCP](FOREMAN.md#mcp)). Until Foreman has a board view, a question an agent asks you shows in `factory ticket list` and is answered with `factory ticket answer`.

## Pipeline

```
door → triage → ready → working → review → security → [public] → gate → ready_for_pr → pr_open → done (merged)
                  ↑         review: changes (once) ←┘         │      └→ needs_human on any failure
                  └──────── public review: block (once) ←─────┘   (public: hygiene projects only)

finding task: ready → working → verifying (verifier) → needs_human (findings ask) → done
                                     each finding filed → triage (change) or ready (intent, designer)
```

- Each stage is one fresh agent; it writes `~/report.json`, which the dispatcher reads.
- A reviewer or security agent that changes the branch is discarded.
- Only the worker and the gate run the whole suite. The worker runs the project's tests and lint before and after its change and must leave them passing; a reviewer runs the lint and, for a project whose kind's toolchain is `go`, `go vet` and `go test -count=1` only on the packages its diff touches (each changed file's package, test data and embedded files included; the whole suite when `go.mod` or `go.sum` changes), or the project's own test command for any other kind; security runs nothing and reads the change. The "This project" facts every agent gets keep the commands. The reviewer's and security's notes come last in their prompts, after the project context: `context/` is one set every role reads, so a note there such as "run the full suite" is for the worker, and the role's note says it holds over it.
- Review sends a change back to the worker once; a second "changes" goes to `needs_human`. The count restarts when a review passes, so a rework started by "changes requested" on GitHub gets its own retry.
- Security tier from the diff, mechanically: 0 text · 1 inert code · 2 I/O boundary · 3 build/deps · 4 several. Patterns per project: `tier3_paths`, `text_paths`.
- Security: the lead gives each angle question to its own sub-agent; the dispatcher (not the agent's verdict) fails the stage if any question is unanswered or rated medium/high.
- Public review (`hygiene = true` projects only; template `public`, `[models] public`, else the security stage's model; no memory, read-only like security): after security, an agent reads everything the change would publish before anything is pushed: every commit, message and patch (`git log -p --text <base>..HEAD`, so a line a later commit removes still counts), and every free text the PR carries, which the dispatcher puts in its prompt: the branch, the PR title, the issue it fixes, the worker's summary, tests and lint, the review's verdict and runtime-behaviour answer, and `[identity] pr_footer`. It is also told the private values: the `[hygiene] deny` terms; the names of the other `[projects.*]` and their `github` repositories; the `[hosts.*]` and the hosts their `ssh` reaches; the `[remote.<name>]` tables; the kinds only config defines; and the people config names (`[board] people`, `[github] reviewers`, `[foreman] tailscale_users`), less the ticket's own project and its repository, `[hygiene] allow`, and any name the repository already has at its base (`git grep`). It reports `pass` or `block` with findings, each a `where` (a commit and path and line, a commit's message, or one of the texts by name; any other `where` makes the report one that cannot be read) and a `why`, never the text itself; the report stays on the board and never reaches GitHub. A pass records the commit it read (`public_head`, beside `texts_before_public`, a digest of the texts it was shown) and goes to the gate. A block sends the change back to the worker once, with the findings, to rewrite its own commits; a second block (counted since the PR last opened), or a finding in the branch, a PR title that is the ticket's own (one made from the summary is the worker's), the issue line or the footer (which the worker cannot change), waits on the desk: send it back with a note, say it is safe to publish at the commit public review read (a person's word; recorded as its pass, with who said so), or drop it. A report that cannot be read is read again; a second in a row stops as a run that left no report (`no_report`), to retry or drop, and is no rework. The gate never checks a public project's change whose branch is not at its `public_head`, or whose PR texts changed since that read (it is read first, so a hand move cannot skip the read), and `OpenPR` pushes nothing for one: it sends it back to the gate (`public_unread`) to be read, so nothing reaches GitHub unread and a ticket moved to `ready_for_pr` by hand is not stuck there. On a rework of an open PR the earlier commits are already on GitHub: a block can stop the new ones, not take those back.
- Gate 1, no model: tests + lint on a fresh clone with no network, allowed paths, size, reviewer's runtime-behaviour answer, worker's stated category and reasoning → `auto_eligible`, else the blockers are listed.
- Early checks: when a worker leaves a head for review, the tick starts Gate 1's checks of that head at once and goes on, so they run beside review, security and public review (not for a paused or board project, nor for a heavy gate, whose `gate_memory` is above `[gate] heavy_memory`, nor when a pull host has `"gate"` in its `prefer`, since the gate would run there). Their container takes one of this host's `max_agents` slots, as a gate's does: they start only when a slot is free, and while they run (from the moment the tick starts them) the tick counts them as one. They are `factory early-checks T… <head>`, started as the transient user unit `factory-gate-early` (at most 70 minutes; the unit gets the tick's `PATH` and `FACTORY_*` settings but its GitHub token; with no user bus they do not start, and the gate runs the checks): the same checks as Gate 1's, on a fresh checkout of their own (`gate/early-<id>`), in the gate image with no network, lent the build cache as Gate 1's checks are. One runs at a time: systemd starts no second unit of that name (that ticket's checks then run at the gate), and a lock in `~/factory/gate-early/` holds for one started by hand. They read the ticket's file (its project and branch) but never change it, and hold no tick lock; their git only verifies the checkout. Only a pass is kept: they ran beside the agents, on a busier host than Gate 1's, so a failure (a timing test, the memory limit) may be the crowding's, and Gate 1 then runs the checks itself. The pass (and the output's tail) is kept in `~/factory/gate-early/<id>.json` (mode 600) under a key: the project, the branch's and the base's heads, the gate image's id (not its tag, which a rebuild moves), the checks' command, and a digest of the rest of the container's setup (the image tag, the gate memory, `[gate] env` and `mounts`, and the pinned files as they are in the checkout: path, mode, size, time). Checks that did not finish keep nothing, and neither do checks whose key changed while they ran. At the gate column Gate 1 takes the heads as always and uses the kept result only when its whole key is the same now: it then runs no checks, so it needs no free slot and sends no gate job, and decides as for checks that ran elsewhere (the changed files, size, walls, hygiene, the move, the PR update and the audit), with the output in `gate-output.txt` headed by when the checks ran early. While early checks of the branch's head still run, the gate waits for them rather than run the same checks beside them. Any other result, or none, and Gate 1 runs the checks as before; so does a gate run again for a head a gate already decided, and a gate that decides removes the ticket's kept result, so checks that finish after it are never used. A result is removed once the gate has looked at it, used or not; one nobody looked at is removed after 7 days, when the next is kept. A `[gate]` mount or a Scion template volume that reaches `~/factory/gate-early` is refused as one that reaches the build caches is (and `factory check` reports such a `[gate]` mount), so neither a branch's tests nor an agent can write a result. Deleting the directory is always safe.
- In `pr_open`, an approving review on GitHub by one of `[github] reviewers`, of the commit the gate checked, is the go-ahead: the next tick merges it (changes requested send it back for a rework). Each tick records the latest such decision on the ticket (`pr.review`). Where GitHub's rules want an approval before a merge (a PR the App pushed needs one from someone else), the desk asks "Approve PR #n on GitHub" and offers no "Merge it" until that commit is approved; a merge GitHub refuses for want of an approval is dropped rather than retried, with `pr.merge_blocked = "approval"` and "waiting for your approval on GitHub" on the ticket.
- Audit: `logs/<ticket>/audit.md` (and `factory audit T…`): door, triage, worker category and reasoning, review, each security answer, public review's verdict and findings (where and why), which gate check decided.
- GitHub-door tickets wait in `needs_human` after triage unless `[triage.auto_accept] github = true`.
- At most `max_agents` at once; each container is capped; one flock per tick. A gate (and the merge check before a merge) takes one of those slots while it runs (so do the early checks while they run; a gate decided from their result runs nothing and takes none); one whose `gate_memory` is above `[gate] heavy_memory` (default `1200m`) waits until no agent runs here, and no new agent starts here while it waits, unless a pull host with `"gate"` in its `stages` or `prefer` takes it as a gate job (see "Gate jobs"). The weekly rebuild skips the checks of paused projects (`enabled = false`, or paused from Foreman).

- Intents (`--intent`): a designer (role `designer`) writes a short design and proposes up to 6 tickets, each standing alone on the base branch. It waits in `needs_human`; `approve` files the tickets into `ready` (skipping triage), `redirect` re-runs the designer with your note and its previous design.
- Prospecting (`[[schedules]]` with `kind = "prospect"`): a prospector proposes at most 5 evidence-backed improvements, never code. In `voting`, `[panel] voters` separate voter agents score every proposal 1–5 for worth, each blind to the others. The dispatcher drops a proposal unless ≥2 voters scored it 4+ or the mean is ≥3.5. Survivors wait as `proposal` tickets; `factory proposals` lists them by mean minus half the spread, and `factory approve` files one as a change.
- Findings (`findings = true` on a task `[[schedules]]` or `[[recipes]]` entry, or `add --task --findings`): a check that looks for problems (bug hunt, repo hygiene, dependency review) reports each one in `findings` beside its `result` (at most 12: `title`, `severity` low/medium/high, `kind` bug/hygiene/compat/security/docs/other, `behaviour_change`, `repro`, `evidence`, `suggested_fix`, `related`). A task done with findings on a factory project goes to `verifying`, where a verifier (role `verifier`, `[models] verify`, any host, no memory, no branch, never commits) re-runs each one on the base branch and checks it against the project's open tickets and its open GitHub issues and PRs, which the dispatcher reads without credentials and puts in the prompt. Its report gives each finding a verdict (`confirmed`, `not_reproduced`, `duplicate` of what), its own evidence, and a title and body for a ticket. The hunt then waits in `needs_human` with the findings ask: every confirmed finding ticked, filed as a change (into triage) or, when it changes behaviour, as an intent (to its designer); the rest greyed with their verdict. Filing (desk, `factory findings T --file`) files each picked finding once (a ticket's `found_in` names its hunt and finding, so an interrupted filing never files one twice), links both ways and closes the hunt, listing what it filed; `--none` closes it with nothing filed. Finding text came from agents that read issues and the web: filed tickets come in through the `finding` door, untrusted (triage rewrites a change in its own words; memory changes are flagged), and the verifier is told so. `[findings] auto_file = "confirmed-safe"` files confirmed findings that change no behaviour as changes at once, and only the rest wait on the desk (`"none"`, the default, files nothing by itself). A finished task whose findings are only prose (from before) gets "Verify these findings" on its result (`factory findings T --verify`): the verifier splits the prose into findings first.
- Tasks (`--task`): one agent, no branch, review or gate; the role writes `result` and the ticket lands in `needs_human`. A helper with `prefer = ["task"]` takes them first. `factory ask` files one about a ticket: the body carries the ticket's facts, a helper checks out the ticket's branch (`ref`), and Foreman shows the question and answer in that ticket's story. A `[[schedules]]` entry with `kind = "task"` files a recurring check that reports back (e.g. repo hygiene); `{github}` in its body names each project's repository.

## Foreman

A web app for the maintainer (`foreman`, one static Go binary). The desk lists every ticket that needs you as a question with its evidence, answer buttons and "How it got here"; the floor is the week's log by day; boards list tickets by column. Answers run in its own process, through the same code as `factory move`, `approve`, `redirect` and `note`; opening, drafting and merging a PR are left to the tick it starts. It listens on loopback (`[foreman] listen`) behind a shared secret; reach it from elsewhere through `tailscale serve`. Deploy installs a sandboxed user unit (it cannot read the App key or the Claude token, and writes only the board, the logs and `~/factory/foreman-state/`); enable it once on the host, as `factory`: `systemctl --user enable --now foreman`. Where the host's user manager cannot apply that sandbox (deploy warns: Foreman could then read the App key and reach the user manager), run `sudo bash host/foreman-system.sh` once: it makes Foreman a system unit under the factory user with the full sandbox (its own user namespace, no `/run/user`, only the token store from `~/.config/factory`), restarted by a path unit when deploy replaces `bin/foreman` or `foreman.env`; deploy then leaves the user unit off. `--undo` reverses it. Its ⚙ page shows the maintainer the host's load, memory, slice, disks, containers and hosts, and each worker host's vitals, from `~/factory/sysinfo.json`, which every tick writes (Foreman's sandbox cannot see `/proc`). Its Live tab shows the local agents' terminals through the screen relay: while Live is open Foreman touches `~/factory/foreman-state/watching`, and `factory-screens.path` (a user unit deploy installs and enables) starts `factory screens` outside the sandbox, which writes each running local agent's `scion look` to `~/factory/screens/<agent>.txt` every 2.5 s and exits 20 s after the last touch. See [`FOREMAN.md`](FOREMAN.md).

## Upkeep

- OS: `sudo bash host/updates.sh` (optionally `REBOOT_AT=...`) lets unattended-upgrades take `-updates` as well as security. It adds a weekly reboot window that reboots only when an update asked for one. Before rebooting it drains new work and waits for agents, image rebuilds and apt.
- Images: `factory rebuild` (timer `factory-rebuild.timer`, `[upkeep] rebuild_at`) rebuilds each agent image this host has, at the versions pinned in `[images]`, with fresh OS packages. A new image replaces the old only after a smoke test and, for the gate image, the projects' tests on their base branch. The old image stays as `:previous`.
- Go: the agent images (`images/claude/Containerfile`, `images/pi/Containerfile`) and the Lever worker image (`images/lever/Dockerfile`) carry the Go toolchain `build.sh` pins, so the factory's own core can be worked and gated like any project (`GOTOOLCHAIN=local`, `GOPROXY=off`, a fresh build cache per container unless the container is lent the trusted one below). Changing the Go pin means changing `go.mod`'s `toolchain`, `build/go.sha256` and all three image files' `GO_VERSION`/`GO_SHA256` in one commit; `TestImagesPinTheBuildToolchain` fails otherwise.
- Go build caches: each host keeps one trusted, compile-only build cache per `go`-toolchain project in `~/factory/cache/go/<project>/`: a generation per base head, `<commit12>/`, and `current`, a link to the newest. A generation is written only by a warm, when a run for that project finds none for the base branch's head (the first run after a merge pays; later ones are warm): a container of its own with no credential, no network, no capability and the source read-only, which compiles the base head without running any of it (`go vet ./... && go test -count=1 -exec /bin/true ./... && go build -o /dev/null ./...`) into a scratch directory seeded from `current`. Only a warm that succeeded is renamed into a generation, and `current` moves to it by a rename; generations other than `current` and the one before are removed, except one a run still has mounted (each run holds a shared lock on `.lease-<commit12>` beside it until its container is gone). Every other container gets the generation `current` names mounted with podman's `:O` at `/home/scion/.cache/go-build` (`GOCACHE` points there), so whatever an agent or a test writes goes to a throwaway upper layer and never reaches the cache or another run. Go keys a package's entries by its directory, so a warm compiles where its consumers do: `/job/repo` on a helper, `/work` for the gate.
  - Where: the master warms its own for its gate (Gate 1's checks, warmed from the base head the gate took, within what the tick's deadline leaves beside the checks' 30 minutes, else lent the cache as it is; the merge queue's checks are lent it as it is, and the rebuild's and the trial's checks in a candidate image run cold, so the live image's compile output cannot hide what the candidate breaks), from a fresh clone of that commit. An ssh helper warms its own for its jobs, from the job's bundle, when the job's `meta.json` names the project, its kind's toolchain and the base head (a dispatcher that predates them sends none, and the job runs cold); the warm takes at most a quarter of the job's time (and 30 minutes) and the agent's container gets what it left, so the two keep to the one timeout the master counts from dispatch, and the warm's output is `jobs/<job>/warm.log`. Pull hosts and local Scion agents run cold: Scion's volumes are plain bind mounts with no overlay, and go fails now and then on a read-only cache.
  - Warming is one at a time per project per host (`.lock`, never waited on): a run that cannot have the lock, or whose warm fails, gets `current` as it was (warmed from an earlier base head), or runs cold with none. Nothing about a cache fails a run.
  - No job or gate may mount the caches otherwise: a `[remote]`, `[remote.<host>]` or `[gate]` mount whose source is `{root}/cache/go`, inside it or above it (the root, `/home`, `/`), written with `{root}` or as an absolute path, is refused by the helper and the gate when they run, for every job and gate on that host whatever the project's kind. This is new: a mount that holds the factory root, which used to work, now stops every job there. `factory check` refuses such a `[gate]` mount and one written with `{root}`, and warns for an absolute `[remote]` one that reaches this host's root or `{factory_home}/factory` (a helper's own root is not known on the master). `factory render` refuses a Scion template whose `volumes:` reach them the same way, so no local agent mounts them either.
  - Deleting `~/factory/cache/go` (or one project's directory) is always safe: the next run warms again.
- Versions: `factory clock` runs `factory versions` weekly and files a `needs_human` notice (project `factory`) when a newer Claude Code or Pi release exists.
- Latest-toolchain trial: every `[upkeep] latest_every_days` (default 30; `0` turns it off), the weekly rebuild on the host with the checkouts also builds a trial of the gate image (`<repo>:trial`) with the newest Crystal and ameba, when one is newer than `[images]` and an enabled project has the crystal toolchain. It runs every project's checks in the trial, runs each failure again in the live image, and files one notice, `Crystal <version> trial`, listing the projects that pass, break and were already failing, with their tails (log: `logs/trial-claude.log`); while it is open, a later trial of the same release files no second one. A trial that stops on an error before every project ran files `... trial could not finish` and is tried again the next week. The trial never tags anything, so the live image never changes; take the new version by setting its `[images]` pin. While the trial is on and a gate image is configured, `factory versions` leaves the Crystal and ameba notices to it; a trial that finds the gate image missing on this host files them itself. `factory rebuild --latest` runs a trial now. Ruby comes from the distro with no pin, so it has no trial.
- Timers: `sudo bash systemd/install-units.sh --enable` on the dispatcher; `--helper --enable` on a helper (rebuild only; `--helper` also deletes any dispatcher units, so the reboot window cannot start a tick there).
- Watchdog: a helper can watch the master from outside. `factory-remote watchdog` (local only, never over ssh) checks `[remote.<this host>] watchdog`, the master's `https://…/healthz`, with no redirects, 10 s and 64 KiB at most; `watchdog_ip` dials that address while TLS still checks the URL's host. The master is down when there is no `ok`, or its `tick-age` is over 2 h or unknown. After two failures in a row it pushes "Factory master down" to the maintainer's devices, again every 6 h while it stays down, and "Factory master is back" once on recovery; its state is `~/factory/watchdog.json`. It pushes with a copy of Foreman's `foreman-vapid.pem` and `foreman-push.json` in `~/.config/factory/watchdog/` (copy them from the master's `foreman-state/` by hand, again after adding a device), which it only reads: whoever holds that key can push to those devices, but read nothing. It only notifies: from a helper a dead master and a network split look the same, so it never takes over. `sudo bash systemd/install-units.sh --helper --watchdog --enable` arms its 5-minute timer; a run without `--watchdog` removes it.
- Backups: once the build has `factory backup`, deploy installs `factory-backup.timer` (daily) and `factory-backup-verify.timer` (a Sunday restore test) as user units, and enables them once `~/.config/factory/backup-key` exists (`factory backup keygen` makes it and prints it once: keep it elsewhere). Deploy never writes the key. The archive holds the board, memory, context, config, the token store's hashes, Foreman's state, `usage.jsonl`, the logs and `repos/<project>/`: the commits of `bot/` branches no remote has, as `unpushed.bundle` (restore into a fresh clone with `git fetch repos/<project>/unpushed.bundle 'refs/heads/*:refs/heads/*'`), and the pinned untracked files under `untracked/`. The clones themselves come back from GitHub. Retention is `[backup] keep_daily`, `keep_weekly` (Sundays) and `keep_monthly` (1sts). Archives are `factory-<name>-<stamp>.tar.gz.fbk`, where `<name>` is `[backup] name` (a slug; default the host name): retention and `verify` see only that line, so naming it keeps one line across a move of the master. `factory backup restore ARCHIVE --to DIR` unpacks an archive into an empty directory outside the root; `factory backup adopt DIR` then moves it into a fresh master's root by rename (same filesystem: restore under the factory user's home). Adopt refuses a root that has a board, a running tick, or any target that is not missing or empty, all checked before anything moves; the hand-copied `foreman-secret` already in `foreman-state/` is kept, but any other `foreman-state/` file in both the root and `DIR` refuses it (Foreman has run there, and its own push key would unbind every device: move those aside first). `config.toml`, `images.json` and `repos/` stay in `DIR`. If adopt fails partway, its error lists what moved and what is still in `DIR`: move the rest into the root by hand, since a second adopt refuses a root that has a board.

## Config

`config.toml` keys: `image`, `max_agents`, `agent_timeout_minutes`, `[identity]` (commit identity, PR footer), `[host]` (factory user/uid/home), `[templates]` (stage → Scion template name), `template_flavour`, `[models]` (stage → model on every host; `public` falls back to `security`'s), `[gate]` (`max_changed_lines`, `allowed_paths`, `memory`, `heavy_memory`, `mounts`, `env`), `[triage.auto_accept]`, `[images]` (`claude`, `pi`, `crystal`, `ameba` versions), `[upkeep]` (`rebuild_at`, `latest_every_days`), `[panel]` (`voters`, `models`: one per voter, in turn), `[foreman]` (`listen`, `hosts`, `trust_tailscale`, `tailscale_users`), `[hosts.<name>]` (`ssh`, `slots`, `stages`, `prefer`, `notes`, `model`, `key`; `transport`: `ssh` or `pull`, with `claim_minutes` and `beat_minutes` for a pull host), `[remote]` and `[remote.<hostname>]` (`agent`, `auth`, `image`, `memory`, `cpus`, `mounts`, `env`, `max_jobs`, `deps`; read on the helper), `[board]` (`people`: whom tickets are assigned to and questions put to, the maintainer first), `[projects.<name>]` (`mode`: `factory` or `board`; `enabled`: `false` pauses it, unless switched on from Foreman (see "Paused projects"); `kind` (see "Project kinds"); `path`, `github`, `base`, `language`, `install`, `test`, `lint`, `notes`, `gate_memory`, `pinned_untracked`, `tier3_paths`, `text_paths`, `walls` (fnmatch patterns of the files a change to which never merges without the maintainer), `hygiene` (the repository is public: scan what a PR publishes against `[hygiene] deny`, and read it in public review first), `self_deploy` (`core` or `config`: a merge here deploys that half of the factory; at most one project each); every agent on the project is told its `language`, commands and `notes`), `[kinds.<name>]` (`from`, `toolchain`, `language`, `install`, `test`, `lint`, `gate_memory`, `pinned_untracked`), `[findings]` (`auto_file`: `none` or `confirmed-safe`), `[hygiene]` (`deny`: the terms a public repo's changes must not carry, each 3 characters or more; `allow`: names public review is not told are private, such as a project or host named with a plain word), `[[schedules]]` and `[[recipes]]` (`findings = true` on a task: its findings are verified and filed through the desk; `projects` limits one to some projects; a schedule is due per project, `every_days` after it last filed there, and never on a project that is paused or has `clock = false`, even one it names, so a project switched on gets its schedules on the next daily clock; the runs are kept in `state.json` under `schedules`). `FACTORY_ROOT` overrides `~/factory`; `FACTORY_API_TOKENS` the token store.

A config directory may also hold `context/<project>/` (an `INDEX.md` plus topic files): the maintainer's notes on that project. `deploy.sh` ships it to `~/factory/context/`; every role with memory on the project finds it read-only in `<memory>/context/` (at most 512 KB; what a run changes there is discarded).

### Project kinds

A factory project's `kind` names its toolchain and supplies the defaults for the keys its own table leaves out: `language`, `install`, `test`, `lint`, `gate_memory` and `pinned_untracked`. A key the table sets always wins, even `pinned_untracked = []`. Core has five kinds, none of which sets `gate_memory` (so `[gate] memory` applies):

| kind | toolchain | language | install | test | lint | pinned_untracked |
| --- | --- | --- | --- | --- | --- | --- |
| `ruby-gem` | ruby | Ruby gem | `bundle check` | `bundle exec rake test` | `bundle exec standardrb` | `Gemfile.lock` |
| `rails-gem` | ruby | Ruby gem for Rails | `bundle check` | `bundle exec rake test` | `bundle exec standardrb` | `Gemfile.lock` |
| `crystal-shard` | crystal | Crystal shard | `shards check \|\| shards install` | `crystal spec` | `crystal tool format --check src spec && ameba src spec` | `lib`, `shard.lock` |
| `go-module` | go | Go module | `go version` | `go vet ./... && go test -count=1 ./...` | `gofmt -l .` must print nothing | none |
| `docs` | none | Config and documentation | `true` | `true` | `true` | none |

`docs` is for a repository of config, notes and documents: there is nothing to build, so its commands pass and the review stages (and public review, with `hygiene = true`) do the work.

`[kinds.<name>]` overrides a built-in kind key by key (`[kinds.ruby-gem]` with only `lint` keeps the rest), or adds a kind: `from` names the built-in it starts from, or it sets its own `toolchain` (`ruby`, `crystal`, `go` or `none`). There is one level: `from` must name a built-in.

```toml
[kinds.framework-shard]
from = "crystal-shard"
gate_memory = "1800m"

[projects.example_shard]
kind = "framework-shard"
path = "/home/factory/src/example_shard"
github = "example-org/example_shard"
base = "main"
```

A project with no `kind` behaves as before: nothing is filled in, and its toolchain (which picks the gate workflow's Crystal setup and the Crystal and ameba version notices) is inferred from `language`: `crystal-shard` when it says "crystal", `go-module` when it starts "Go module", else `ruby-gem`. `factory check` reports an unknown `kind`, a bad `toolchain` or `from`, and a kind with no toolchain. `factory kinds [--json]` lists every kind (where it comes from, its toolchain, its `from`) and, for each factory project, its kind (`(inferred)` when it has none), toolchain and the effective language, commands, gate memory and pinned files.

`factory onboard <project>` checks whether a factory project is ready to switch on, and prints one line per check: `kind` (a warning when inferred), `commands` (install, test and lint, from the table or the kind), `checkout` (the base's head in `path`), `context` (`context/<project>/INDEX.md`; a warning), `github` (the App can reach the repository; a warning with no `github`), `rules` (the base requires pull requests; a warning otherwise, or when the rules cannot be read) and `main`. Each is `ok`, `warn`, `fail` or `skip` (with the reason); it exits 1 when one fails. It only reads, except with `--checks`, which runs the gate's checks on the base's head in the gate image, holding the tick's lock (it waits for a running tick); without it `main` is skipped. `--json` prints the report as one object (`project`, `kind`, `inferred`, `ok`, `items`: `check`, `status`, `detail`) for the operator.

## Tests

```sh
go vet ./... && go test ./...
GIT_AUTHOR_NAME=x GIT_AUTHOR_EMAIL=x@example.com GIT_COMMITTER_NAME=x GIT_COMMITTER_EMAIL=x@example.com go test ./...   # as inside an agent container
./build.sh --verify-repro          # two builds in different directories are byte-identical
FACTORY_SANDBOX_PROBE=1 go test -run TestForemanSandboxOnThisSystemd .   # Foreman's sandbox, through this machine's user manager
```

Tests that run git never depend on the caller's git identity: the agent containers set `GIT_AUTHOR_*` and `GIT_COMMITTER_*`, and the shared helpers (`testkit.Root(t).Environ()`, `testkit.NewRepo`, `testkit.WithoutGitIdentity` in the packages' own git helpers) drop the names, emails and dates and set their own identity where a test needs one.

The goldens under `internal/*/testdata/py/` (and the other fixtures that name Python as their source) were written by the Python factory, the test oracle until the cutover deleted it; they are static fixtures now, and the generators are in the git history only. Where Go has outgrown Python (the public review column), the golden is Go's own: a regression golden written from Go's output and checked in, `internal/model/testdata/model.json` and `internal/render/testdata/example/`, and the tests that still compare with Python's leave that column out. A few tests read a private snapshot of the live board and skip without it: `tools/capture-live.sh DEST` takes one (into a directory outside this repository), and `FACTORY_LIVE_SNAPSHOT=DEST go test ./internal/...` runs them.

There is no CI: these checks, `go test -race ./...` and the reproducibility check are run by hand (on the desktop) before a merge to main.

## License

MIT

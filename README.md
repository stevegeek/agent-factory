# agent-factory

Orchestration for unattended coding agents on small, resource-constrained machines. A file-based board drives each ticket through fresh, sandboxed agents (triage → work → review → security) and a deterministic gate, then opens a pull request through a GitHub App. Agents never push; the dispatcher pushes only `bot/*` branches, and merging stays with a person.

Agents: Claude Code via [Scion](https://github.com/GoogleCloudPlatform/scion) (local mode, rootless Podman), or [Pi](https://www.npmjs.com/package/@earendil-works/pi-coding-agent) for hosts whose CPU can't run Claude Code's native binary.

## Layout

| Path | |
| --- | --- |
| `bin/factory` | dispatcher (Python 3 stdlib): board, pipeline, gate, doors |
| `bin/render` | renders host files from a config dir |
| `bin/pi-run` | runs Pi in a capped container |
| `bin/factory-remote` | on a helper host: runs dispatcher stages as Pi jobs (the dispatcher's SSH key is forced to it) |
| `foreman/` | the maintainer's web app (Go, stdlib only): desk of asks, weekly floor, boards |
| `deploy.sh` | render + copy to a host over SSH (builds foreman when `go` is present) |
| `host/setup.sh` | host base setup: zram, systemd-oomd, locked `factory` user, memory budget, rootless Podman |
| `host/slim.sh` | optional: strip the Ubuntu server install to what the factory uses (dry run by default) |
| `host/allow-dispatcher.sh` | on a helper host: authorise the dispatcher's key, forced to `factory-remote` |
| `images/claude/` | Scion/Claude Code agent image + rules every agent gets |
| `images/pi/` | Pi agent image |
| `templates/<flavour>/` | Scion role templates (`ruby-gem` ships) |
| `systemd/` | tick / sweep / clock timers |
| `scion/patch_provision.py` | local-mode trust-path workaround for Scion's Claude harness |
| `examples/config/` | starting config |

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
core/deploy.sh --config . --host factory@host
```

On the host, as `factory`:

```sh
# Claude Code: build sciontool + scion, then the image
podman build -t localhost/factory-agent:scion ~/factory/images/scion-agent   # needs ./sciontool in that dir
claude setup-token        # save the printed token to ~/.config/factory/claude-oauth-token, mode 600
# Pi: Anthropic API key (subscription OAuth is for Anthropic's own apps only)
podman build -t localhost/factory-pi:dev ~/factory/images/pi
umask 077; cat > ~/.config/factory/anthropic-api-key
```

Timers, as admin: `sudo bash ~factory/factory/systemd/install-units.sh --enable`

### Helper hosts

A smaller second host can run stages as Pi jobs: as overflow for the stages it lists when the dispatcher's own slots are full, or for any ticket pinned to it (`--host`). On the dispatcher, as `factory`:

```sh
ssh-keygen -t ed25519 -N '' -C dispatcher -f ~/.ssh/factory-remote
ssh-keyscan HELPER_IP >> ~/.ssh/known_hosts      # compare with the helper's host key
```

On the helper, after `setup.sh`, a deploy, the Pi image build and its API key, as admin:

```sh
sudo bash host/allow-dispatcher.sh --from DISPATCHER_IP --key "$(cat factory-remote.pub)"
```

Then `[hosts.<name>]` in the config (see `examples/config/config.toml`). Jobs get the same `report.json` contract; a worker's commits come back as a git bundle, and a reviewer's job reports the head it left so a moved branch is caught. The helper's `[remote]` table, not the dispatcher, decides image, memory, mounts and env.

## Use

```sh
factory add <project> "Title" --body "what and why"   # human door
factory sweep                                          # github door: open issues → triage (untrusted)
factory clock                                          # clock door: [[schedules]] that are due
factory ls [--all] | show T0001 | move T0001 ready --note "approved"
factory host T0001 helper                              # pin to a host (local, a [hosts] name, or auto)
factory add factory "Summarise <url>" --task           # one agent run, answer comes back to you
factory result T0009                                   # (project `factory` = no repository; --timeout MIN)
factory add <project> "Idea" --intent --body "..."     # design first: a designer proposes
factory result T0010                                   # read the design + proposed tickets
factory approve T0010 [--only 1,3] | factory redirect T0010 --note "..."
factory tick                                           # one pass; the timer runs it every 5 min
factory github                                         # check the GitHub App installation
factory pr T0001                                       # push bot/* + open the PR (draft unless auto-eligible)
factory fizzy                                          # sync the Fizzy boards now and list them
pi-run --workdir DIR -p "task"                         # one Pi agent; --model / PI_MODEL to override
```

## Pipeline

```
door → triage → ready → working → review → security → gate → ready_for_pr → pr_open → done (merged)
                  ↑         review: changes (once) ←┘     └→ needs_human on any failure
```

- Each stage is one fresh agent; it writes `~/report.json`, which the dispatcher reads.
- A reviewer or security agent that changes the branch is discarded.
- Security tier from the diff, mechanically: 0 text · 1 inert code · 2 I/O boundary · 3 build/deps · 4 several. Patterns per project: `tier3_paths`, `text_paths`.
- Security: the lead gives each angle question to its own sub-agent; the dispatcher (not the agent's verdict) fails the stage if any question is unanswered or rated medium/high.
- Gate 1, no model: tests + lint on a fresh clone with no network, allowed paths, size, reviewer's runtime-behaviour answer, worker's stated category and reasoning → `auto_eligible`, else the blockers are listed.
- Audit: `logs/<ticket>/audit.md` (and `factory audit T…`): door, triage, worker category and reasoning, review, each security answer, which gate check decided.
- GitHub-door tickets wait in `needs_human` after triage unless `[triage.auto_accept] github = true`.
- At most `max_agents` at once; each container is capped; one flock per tick.

- Intents (`--intent`): a designer (role `designer`) writes a short design and proposes up to 6 tickets, each standing alone on the base branch. It waits in `needs_human`; `approve` files the tickets into `ready` (skipping triage), `redirect` re-runs the designer with your note and its previous design.
- Prospecting (`[[schedules]]` with `kind = "prospect"`): a prospector proposes at most 5 evidence-backed improvements, never code. In `voting`, `[panel] voters` separate voter agents score every proposal 1–5 for worth, each blind to the others. The dispatcher drops a proposal unless ≥2 voters scored it 4+ or the mean is ≥3.5. Survivors wait as `proposal` tickets; `factory proposals` lists them by mean minus half the spread, and `factory approve` files one as a change.
- Tasks (`--task`): one agent, no branch, review or gate; the role writes `result` and the ticket lands in `needs_human`. A helper with `prefer = ["task"]` takes them first.

## Fizzy

With `[fizzy]` in the config and a token in `~/.config/factory/fizzy-token`, each tick mirrors the board to [Fizzy](https://fizzy.do): one private board per project (`factory: <project>`), one card per ticket, new history as comments. The file board stays the source of truth; `fizzy.json` maps tickets to cards.

| Ticket column | Card |
| --- | --- |
| triage | Maybe? |
| ready, working, needs_human, ready_for_pr, pr_open | Ready, Working, Needs you, Ready for PR, PR open |
| voting, review, security, gate | Checks |
| done / rejected | Done / Not Now |

The token owner can drive tickets from the cards:
- Moving a card to Maybe?, Ready, Needs you, Done or Not Now moves the ticket.
- Moving an intent or proposal from Needs you to Ready approves it.
- A move into a pipeline column, or while an agent runs, is moved back with a comment.
- A comment of `/approve [1,3]`, `/redirect <note>` or `/pr` runs that command; any other comment becomes a note in the ticket's next agent prompt.
- Other people's comments are ignored.

## Foreman

A web app for the maintainer (`foreman/`, one static Go binary). The desk lists every ticket that needs you as a question with its evidence, answer buttons and "How it got here"; the floor is the week's log by day; boards list tickets by column. Answers run the `factory` CLI (`move`, `approve`, `redirect`, `note`, `pr`). It listens on loopback (`[foreman] listen`) behind a shared secret; reach it from elsewhere through `tailscale serve`. Deploy installs a user unit; enable it once on the host, as `factory`: `systemctl --user enable --now foreman`. See `foreman/README.md`.

## Upkeep

- OS: `sudo bash host/updates.sh` (optionally `REBOOT_AT=...`) lets unattended-upgrades take `-updates` as well as security. It adds a weekly reboot window that reboots only when an update asked for one. Before rebooting it drains new work and waits for agents, image rebuilds and apt.
- Images: `factory rebuild` (timer `factory-rebuild.timer`, `[upkeep] rebuild_at`) rebuilds each agent image this host has, at the versions pinned in `[images]`, with fresh OS packages. A new image replaces the old only after a smoke test and, for the gate image, the projects' tests on their base branch. The old image stays as `:previous`.
- Versions: `factory clock` runs `factory versions` weekly and files a `needs_human` notice (project `factory`) when a newer Claude Code or Pi release exists.
- Timers: `sudo bash systemd/install-units.sh --enable` on the dispatcher; `--helper --enable` on a helper (rebuild only).

## Config

`config.toml` keys: `image`, `max_agents`, `agent_timeout_minutes`, `[identity]` (commit identity, PR footer), `[host]` (factory user/uid/home), `[templates]` (stage → Scion template name), `template_flavour`, `[models]`, `[gate]` (`max_changed_lines`, `allowed_paths`, `memory`, `mounts`, `env`), `[triage.auto_accept]`, `[images]` (`claude`, `pi` versions), `[upkeep]` (`rebuild_at`), `[panel]` (`voters`), `[foreman]` (`listen`), `[hosts.<name>]` (`ssh`, `slots`, `stages`, `prefer`, `notes`, `model`, `key`), `[remote]` (`image`, `memory`, `mounts`, `env`; read on the helper), `[projects.<name>]` (`path`, `github`, `base`, `install`, `test`, `lint`, `pinned_untracked`, `tier3_paths`, `text_paths`), `[[schedules]]`, `[fizzy]` (`account`, `board_prefix`, `token`, `api`). `FACTORY_ROOT` overrides `~/factory`.

## Tests

```sh
python3 -m unittest discover -s tests
cd foreman && go test ./...
```

## License

MIT

# agent-factory

Orchestration for unattended coding agents on small, resource-constrained machines. A file-based board drives each ticket through fresh, sandboxed agents (triage → work → review → security) and a deterministic gate. Agents work on branches and never push; anything irreversible stays with a person.

Agents: Claude Code via [Scion](https://github.com/GoogleCloudPlatform/scion) (local mode, rootless Podman), or [Pi](https://www.npmjs.com/package/@earendil-works/pi-coding-agent) for hosts whose CPU can't run Claude Code's native binary.

## Layout

| Path | |
| --- | --- |
| `bin/factory` | dispatcher (Python 3 stdlib): board, pipeline, gate, doors |
| `bin/render` | renders host files from a config dir |
| `bin/pi-run` | runs Pi in a capped container |
| `deploy.sh` | render + copy to a host over SSH |
| `host/setup.sh` | host base setup: zram, systemd-oomd, locked `factory` user, memory budget, rootless Podman |
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

## Use

```sh
factory add <project> "Title" --body "what and why"   # human door
factory sweep                                          # github door: open issues → triage (untrusted)
factory clock                                          # clock door: [[schedules]] that are due
factory ls [--all] | show T0001 | move T0001 ready --note "approved"
factory tick                                           # one pass; the timer runs it every 5 min
pi-run --workdir DIR -p "task"                         # one Pi agent; --model / PI_MODEL to override
```

## Pipeline

```
door → triage → ready → working → review → security → gate → ready_for_pr
                  ↑         review: changes (once) ←┘     └→ needs_human on any failure
```

- Each stage is one fresh agent; it writes `~/report.json`, which the dispatcher reads.
- A reviewer or security agent that changes the branch is discarded.
- Security tier from the diff, mechanically: 0 text · 1 inert code · 2 I/O boundary · 3 build/deps · 4 several. Patterns per project: `tier3_paths`, `text_paths`.
- Gate 1, no model: tests + lint on a fresh clone with no network, allowed paths, size, reviewer's runtime-behaviour answer → `auto_eligible`.
- GitHub-door tickets wait in `needs_human` after triage unless `[triage.auto_accept] github = true`.
- At most `max_agents` at once; each container is capped; one flock per tick.

## Config

`config.toml` keys: `image`, `max_agents`, `agent_timeout_minutes`, `[identity]` (commit identity, PR footer), `[host]` (factory user/uid/home), `[templates]` (stage → Scion template name), `template_flavour`, `[models]`, `[gate]` (`max_changed_lines`, `allowed_paths`, `memory`, `mounts`, `env`), `[triage.auto_accept]`, `[projects.<name>]` (`path`, `github`, `base`, `install`, `test`, `lint`, `pinned_untracked`, `tier3_paths`, `text_paths`), `[[schedules]]`. `FACTORY_ROOT` overrides `~/factory`.

## Tests

```sh
python3 -m unittest discover -s tests
```

## License

MIT

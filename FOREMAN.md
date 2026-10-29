# Foreman

The web app the people who run the factory share: the `foreman` binary, built from `cmd/foreman` and `internal/web` (with `present`, `desk`, `mdhtml`, `auth`, `push`, `guard`, `api` and `mcp`). It reads the board (`$FACTORY_ROOT/board/tickets/*.json`) and shows:

- **Desk** (`/`): every ticket that needs a person, as an ask: one question, the evidence, the answers that make sense, and "How it got here" (history, reports, votes, gate and the rule behind each automatic decision). Each row reads project, title, then what it wants ("Review PR #9", "Approve design · 3 tickets"). An ask whose last event is your own note or question moves down to **You've responded** (with what you did and when) and leaves the count and the badge; another person's note or question leaves it waiting for you; anything new from the factory brings it back up. After any answer, including a note or a question, you are back on the desk (with its kind filter, if one was on). A slow answer (opening a PR pushes to GitHub) shows a spinner and what is happening on the tapped button, and the form takes no second tap. Ask the agent gets an answer from an agent in a few minutes; Save note only keeps the note for the next agent that works on the ticket.
  - *Whose desk*: each person's desk holds the asks of their desk projects (Settings; none ticked is every project), except that an ask put to someone by name is on that person's desk alone, whatever its project: a board question's recipient, else the ticket's assignee, when they are a user (before there are any, one of `[board] people`); an ask put to a name no user has stays on its project's desks. Passing offers `[board] people`, and a pass of a board card an agent is working in Doing is refused. Every board stays open to everyone.
  - *For you*: above the asks, the tickets someone else put in front of you, from any project, newest first: one passed to you ("Pass to someone" on a ticket's page sets its assignee to a person, with an optional note that goes on as a note, or a comment on a board ticket), or a note, comment, board question or answer that mentions `@yourname`. Each stays until you put it away on its page ("Put away") or write on the ticket yourself; something newer brings it back. They count in the badge, and passing or mentioning pushes to that person's devices.
  - *Touches the walls*: a change to a file in its project's `walls` (Gate 1 records them as `gate.walls`) is badged `walls` on its row, its review or open ask reads "Review PR #9 · touches the walls", and its detail lists the files; it never merges without you.
  - *Failed gate*: when the gate's checks fail, the ask's why ends with where they ran and their exit ("Gate on hostx: exit 1") and their last lines as code; the ticket page shows those lines above the answers, and the desk row shows the first one that reads as the failure. A host that lacks a command the checks need is no failed gate: the gate goes to another host, and only when none is left does the ask say which host lacks what ("no host could run p's checks: hostx: go: not found").
  - *Public review*: on a public project (`hygiene = true`) a change waits in the **Public review** column between Security and the gate while an agent reads what it would publish. Its line ("Pass: nothing private in what it would publish at <commit>", "Blocked: 2 findings …", or after a person's word "Passed by <who> at <commit> (public review had blocked it)") is in the evidence, and each run's own report in the story and the thread; a block that needs you reads "Public review blocked it", and lists each finding as where and why, never the text. The findings stay on Foreman and the board.
- **Floor** (`/floor`): the week as a log by day, each line expanding into that ticket's story, including proposals the panel dropped and checks that closed with nothing to do. `?week=YYYY-MM-DD` for earlier weeks. Its **Floor metrics** link opens `/floor/metrics` (the same `?week`): what the factory cost you and how often its work came back, for the week shown and the four before it, in one table for all projects and one (folded) per project with anything to count. Counted from the tickets' histories alone, in one read-only pass (board tickets are not counted, as they are not on the floor):
  - *Your attention*: the asks answered that week, by kind, and the median and longest time an ask waited from its `since` to the answer. An ask opens when a ticket arrives in needs_human, ready_for_pr or pr_open (its kind read from that move's reason, as the desk asked it then) and is answered by the next move when a person made it (`by` names a person, or older history's "by human"; an approval or a redirect), by a `pr` `merge_requested` event ("Merge it"), or by GitHub: the PR merged, closed or with changes requested. A move the factory made on its own (an auto-opened PR, a gate run again) answers nothing. Only your own answers count unless you are the maintainer, who sees everyone's; an answer whose history names nobody (GitHub's, a merge request, older boards) is the maintainer's.
  - *Send-backs*: reviewer rework rounds (`review_changes`), public review's own rework rounds (`public_changes` the factory made), changes requested on GitHub (`changes_requested`), designs redirected (`redirected`), and asks sent back from the desk (a person's move to Ready from a Review, Open PR or Config draft ask, or a person's send-back of a change public review blocked).
  - *Outcomes*: PRs merged (`pr_merged`), PRs closed unmerged (`pr_closed`), tickets dropped by a person (a person's move to Rejected) and closed by triage (`triage_rejected`). Reverts read "not tracked": nothing records a merged PR being reverted yet.
  - *Tokens*, after the weekly tables and up to now whatever week is shown: what the agents spent today (since local midnight), in the last 7 days and in the last 30, read from `usage.jsonl` (README "Token usage"): the total split into input, output, cache write and cache read, the runs, then a row per stage and per project, largest first. A line under the table counts the runs read only in part (a transcript over the reader's caps), whose tokens are a lower bound. Pi runs record nothing, and the numbers are tokens, not money.
- **Boards** (`/boards`, `/b/<project>`): tickets by column. `/t/<id>` shows any ticket. Factory projects come first; board projects (`mode = "board"`, configured ones even with no tickets) follow under "Boards for agents", badged `board`, with their open, doing, waiting and closed counts. A board project's page has a lane for each board column in order (Inbox, Next, Doing, Waiting, Done; Done shows its newest 30 and links search for the rest), cards with their assignee and labels, and Dropped folded with its count. Agents move board tickets through the ticket API and MCP; Foreman never does. A paused factory project (`enabled = false`, no agents, gates, schedules or sweeps) is badged `paused`. Each factory project has a switch: **Pause** or **Switch on**, which overrides config's `enabled` (config stays the default) and wakes the tick; while switched it says "paused here by X at T" (or "switched on here") with **Follow config (enabled|paused)** to go back. The switch is kept in `foreman-state/projects.json`, the one file Foreman writes that the tick reads for this; each change is a line in `foreman-state/foreman.log`, never a ticket event. `factory project disable|enable|follow NAME` does the same from a shell, and `factory check` lists the switches. A board ticket's page shows its column, assignee, labels, who filed it, the body, the result, every question an agent asked with its answer, and the comments and questions in How it got here and the Conversation. A factory ticket's page lists its archived agent runs and, under them, each run's tokens from its `usage.json` (the split on hover) and the ticket's total, split into input, output, cache write and cache read; a run read only in part is marked `+` (a lower bound), and so is the total; a run that recorded none is left out.
- **Board questions**: an agent's open question to a person on a board ticket (`ask_person`) is an ask on the desk like any other ("Answer example-agent", with the project), is pushed, and is answered from the web: an option it offered, a typed answer, or both (the typed text goes under the option). The answer goes through the ticket API's `answer` verb, the one MCP's `answer_question` runs, recorded as by whoever answered; the ticket API moves the ticket back out of Waiting. Nothing else is answered or noted on a board ticket from Foreman. Search covers board tickets, their comments and questions; a ticket id opens it.

Answers and the ticket API run in Foreman's own process, through the same `ops` and `ticketapi` code (and the same locks) the `factory` CLI uses; Foreman's code holds no decisions of its own and cannot reach GitHub, git writes, Scion writes or the gate (the import table in `internal/archtest` makes that a build failure). Work that needs the GitHub App (opening, drafting or merging a PR) is recorded on the ticket and done by the next tick, which Foreman starts at once. Boards from before the dispatcher wrote `rule` and panel votes still work. The desk shows the `ask` the dispatcher stored on each ticket waiting for you; `factory check --asks` lists any ticket in an ask column without one.

One static binary, standard library only, htmx and fonts vendored (Barlow Condensed, IBM Plex; SIL OFL, licences in `internal/web/static/fonts`); no external requests.

An ask reads top to bottom: a short question with the ticket's title under it, a "Waiting for you" line, the evidence (a callout says why a PR is a draft), then the answers (primary first, anything that closes a ticket or PR last and red), the note box with Ask and Send note, and the story with its earlier steps folded.

Open pages stay current: `foreman.js` asks for the page every 30 s (while the tab is visible) with the board version it was rendered from, and the server answers 204 until a ticket changes. A form you have started on is never replaced; if its question moved on, a bar says so and a reload brings the new one. An unsent note is kept in the browser until it goes through.

## Build and run

```sh
go test ./internal/web/... ./cmd/foreman/...
./build.sh --out /tmp/out && /tmp/out/foreman --root ~/factory    # all three binaries, the pinned toolchain
```

`deploy.sh` builds it with the other binaries and installs `systemd/foreman.service` as a user unit. On the host, as the factory user, once: `systemctl --user enable --now foreman`; later deploys restart it.

The unit is sandboxed: Foreman runs as the same user as the tick, so the unit keeps the process itself from the GitHub App key and the Claude token (`InaccessiblePaths`), and makes everything but the board, the logs and `~/factory/foreman-state/` read-only (`ProtectSystem=strict`, `ProtectHome=read-only`, through `PrivateUsers`). It cannot replace the binaries, the source or the config the tick runs. `deploy.sh` applies the sandbox through `systemd-run --user` before it installs the unit, checks that the App key cannot be read under it, and asks `/healthz`, which lists any sealed file the running process can open (there must be none). If the user manager cannot apply the sandbox, deploy installs the unit without it and says so. The sandbox hides podman and the user manager, so Foreman runs no `scion` at all: Live's local screens come from the screen relay (see [Live](#live)). `MemoryMax=128M`: answers, the API and MCP run inside it.

Foreman's own files live in `~/factory/foreman-state/` (mode 0700): the secret `foreman-secret` (and `.since`), `foreman-users.json`, the VAPID key `foreman-vapid.pem`, the devices `foreman-push.json` and `foreman-seen.json`. The first Go deploy moves them there from `~/.config/factory/`. Foreman and `foreman users` default to `~/factory/foreman-state/foreman-secret`, and fall back to `~/.config/factory/foreman-secret` while only the old file exists (a host deploy has not moved yet). `FOREMAN_SECRET_FILE` names another file for `foreman users`.

| Flag | Default | |
| --- | --- | --- |
| `--root` | `$FACTORY_ROOT` or `~/factory` | the board to read |
| `--factory` | | ignored (answers no longer run the CLI; kept so older units start) |
| `--listen` | `127.0.0.1:8790` | `[foreman] listen` in config.toml for the unit |
| `--secret-file` | `~/factory/foreman-state/foreman-secret` (the old `~/.config/factory/foreman-secret` while only that exists) | created (0600) when missing; the unit passes it |
| `--trust-tailscale` | off | accept `Tailscale-User-Login` from loopback for the `--tailscale-user` logins |
| `--tailscale-user` | | a login let in by `--trust-tailscale` (repeatable; required with it) |
| `--host` | | a host name to answer to besides IP addresses and `localhost`, e.g. the ts.net name (repeatable) |
| `--revoke-sessions` | | log out every browser and exit |
| `--project` | | a project a new ticket may name (repeatable; deploy passes `[projects]`) |
| `--push` | on | Web Push to subscribed devices; the VAPID key (`foreman-vapid.pem`) and the devices (`foreman-push.json`) live beside the secret |
| `--api` | on | the ticket API at `/api/v1` and MCP at `/mcp`; see [Ticket API](#ticket-api) and [MCP](#mcp) |
| `--api-tokens` | `~/.config/factory/api-tokens.json`, or `api-tokens.json` beside an explicit `--secret-file` | the dispatcher's token store (`factory tokens`; `FACTORY_API_TOKENS` for the CLI); the unit passes `~/.config/factory/api-tokens.json`, read-only to Foreman |
| `--api-concurrency` | 2 | ticket verbs the API and MCP run at once, together |

## New tickets

＋ (`/new`) files a ticket without GitHub: a project (or `factory` for a question with no repository), a kind (make a change, design it first, ask a question), a title and a Markdown description. It files it as `factory add --body=… [--intent|--task] -- <project> <title>` would; the ticket skips triage and goes straight to an agent. The project list has two groups, and the form says which one you chose:
- **Factory projects:** the pipeline works them.
- **Boards:** filing on one makes a card in that board's Inbox, through the ticket API's `create` verb, as you (`/new?project=<board>` opens on it). The kinds and recipes are hidden then, because no factory agent ever takes a board card.

## Live

Live (`/live`) opens with **In flight**, over every factory project (board tickets and board projects are not in it), so that after answering on the desk you can follow a ticket into the queue, onto an agent and on. It is told from the ticket files, config.toml (with the project switches) and the tick's last `sysinfo.json` alone, in one pass over the board, and updates with the page's own poll, whenever the board changes:

- **Running**: the tickets with an agent on them: the stage (a public review says it is reading what the change would publish), the host, and how long it has run (linked to its screen further down).
- **Waiting to start**: every factory ticket with no agent in a column where the tick starts one (its next stage, as `factory` reads it from the model: triage, ready, review, security, public review, verifying, voting for a prospect) or in the gate (a public project's ticket there with no public review pass waits for public review, which reads it first), oldest wait first: its project, what it waits for, how long since it came into that column, and why. The why is the tick's start loop played over the board in board order, the slots running agents take counted across every project: the project is paused (in config.toml, or switched off in Foreman, by whom and when); it starts at the next tick, here or on which host (a gate as a gate job on a pull host); it waits for a free slot (each candidate host's agents against `max_agents` or its `slots`); a heavy gate (gate memory above `[gate] heavy_memory`) waits for this host to be idle and holds it, so what comes after it waits too; it is pinned to a host that is paused after a usage limit, or a pull host that may be away (its last vitals older than its `beat_minutes`; whether it called in since, only its mailbox knows), or a host that does not take its project's kind (`kinds`, `not_kinds`); or no host runs its stage. A host whose kinds rule keeps the project away is never named as the one that starts it. What only the hosts know (podman's own count, a helper refusing a job, the tick's time left for a gate, a gate's kept early checks, which let it run without a slot, early checks that run now, which take one and hold their head's gate) is not guessed; without a readable config the row only says the next tick takes it. A snapshot older than 15 minutes adds that the tick is not running.
- **Just moved**: the last day's moves, newest first and at most 30: when, the ticket, from and to, and who: a person (you, highlighted, for your own answers) or the factory with the move's words or its reason. Older untyped history reads as well ("by human" is a person's move).

Below it, **Screens**: each running agent's screen, polled every 5 s. It is read-only: nothing on it can type into an agent. The end of `factory.log` merged with Foreman's own `foreman-state/foreman.log` is on **Logs** (`/logs`, in the menu), polled every 10 s.

- **A local agent** (`host = local`): Foreman cannot run `scion look` inside its sandbox, so a relay does. Each screen poll touches `~/factory/foreman-state/watching` (at most once a second). `factory-screens.path`, a user unit of the factory user that deploy installs and enables like `factory-wake.path`, sees the change and starts `factory-screens.service`, outside the sandbox: `factory screens` lists the board's running local agents and runs `scion look <agent> --plain -n 400` for each in its workspace, two at a time, 25 s at most each, every 2.5 s. It writes each screen atomically (its last 400 lines, at most 64 KB, never through a link) to `~/factory/screens/<agent>.txt`, deletes the screens of agents that stopped, and exits once the marker is 20 s old. Foreman reads that file (a valid agent name, no link followed) and shows its age in the header (`screen 3 s old`); while it is missing or over 15 s old (the relay is starting) it shows the end of the agent's `agent.log`, headed `screen starting…`.
- **A helper host's job**: the tail of its output over ssh, as before. **A pull host's job**: the log tail its last beat left in the mailbox. A gate job on a pull host (README "Gate jobs") is listed as a **Gate** run, and its tail is the checks' output so far.

The relay's only input from Foreman is the marker's mtime: it never reads what is in the file, and runs nothing named by anything Foreman writes. It writes only `~/factory/screens/`, which Foreman reads and cannot write (only `board/` and `foreman-state/` are writable to it), and nothing under `logs/` or `foreman-state/`.

## Install and notifications

Foreman is an installable web app (`/manifest.webmanifest`, the service worker `/sw.js`, icons in `static/icons`). On an iPhone: Safari → Share → Add to Home Screen, then open it from the home screen and turn notifications on under Settings, in the menu at the top right (iOS 16.4 or later; push only works in the installed app). The app icon's badge counts the asks still waiting for you.

### The address

Push subscriptions, the session cookie and the installed app are bound to the origin, so give Foreman an address that does not belong to one machine: a Tailscale Service such as `svc:foreman` (`https://foreman.example.ts.net`), served on the master with `tailscale serve --service=svc:foreman --https=443 127.0.0.1:8790`. Moving the master then moves the Service, and no device notices (README, "Moving the master"). List that name first in `[foreman] hosts`: every name Foreman answers to must be there (any other `Host` gets 421), and the first, `hosts[0]`, is the VAPID contact its pushes carry. The machine's own `ts.net` name can stay listed after it while devices move over; manifest, service worker and push URLs are origin-relative, so each origin works on its own.

`/healthz` (no login) answers `ok`, `version <commit>`, `started <time>` and `tick-age <seconds>`: how long ago a `factory tick` process last ran to its end and wrote `sysinfo.json`, or `tick-age unknown` when that file is missing or does not read. A tick that finds another holding the tick lock writes it too (System status shows the host alive through a long tick), and so does one whose pass failed, so the age says ticks still start, not that they get anywhere; a tick stuck holding the lock is stopped by its unit after 45 minutes. A check from outside can tell a master whose ticks stopped from a live Foreman by it. Any `exposed:` line (a sealed file or the user manager in reach) follows.

**GitHub** shows under the top bar on every page, from `~/factory/github.json`, which the tick's GitHub pass writes: "GitHub: connected · last poll <time>", or "GitHub: unreachable since <time> (last error: …)" with the last good poll. A pass fails only when no call to GitHub got an answer (DNS, a refused or reset connection, a timeout, a 5xx, or the installation token failing for one of those); a ticket is not given an error event for that, and is tried again on the next tick. GitHub is unreachable after 2 failed passes in a row and connected again on the first answer; the factory log says each change once. A refusal about the ticket itself (404, 403, 422) is still an error event on it.

Every 30 s Foreman compares the board with what it saw before and pushes: a new ask on the desk, the answer to a question asked with Ask, and a ticket with a PR that closed without you (merged or closed on GitHub). On a board ticket only an agent's new question to a person is news ("example-agent asks on notes: …", opening the conversation). What you did yourself (a move `by human`, or a merge on your go-ahead) is not news to your own devices, only to everyone else's; more than three at once arrive as one. Tapping a notification opens the ticket. Payloads are encrypted for the device (RFC 8291) and signed with VAPID (RFC 8292); a subscription may only point at a known push service (Apple, Google, Mozilla, Microsoft). What it last saw is kept in `foreman-seen.json` beside the secret, so a change made while Foreman was down (a deploy, a reboot) is still announced; a board that fails to read is skipped, not taken as every ticket gone.

## System

**System status** (`/system`, in the menu at the top right; `/system.json` too, also at its first address, `/settings/system.json`). Foreman's sandbox hides the host's `/proc` from it, so every tick ends by writing what it sees to `~/factory/sysinfo.json` (atomically; at most 3 s, and a failure is only a log line): host name, kernel, uptime, load and CPUs, memory and swap, the factory user's slice (`memory.current` against its `MemoryMax` or `MemoryHigh`), disk use of `/` and the factory root, the CPU temperature when `/sys/class/thermal` has one, the running containers (`podman ps`), agents per host against their slots, pauses, `images.json`, each image's last rebuild and its verdict from `factory.log`, and the factory binary's version. Before that the tick asks every `[hosts]` host for its vitals, all at once and within 7 s: a helper answers the `vitals` verb over ssh, a pull host's last post is read from its mailbox; each is stamped with the dispatcher's clock when it arrives, and a host that gives none this pass keeps its last vitals beside the error. The page opens with a summary of every configured host: online (fresh vitals, or a fresh snapshot for the dispatcher), offline or not reported yet, and working, paused or idle. Then it shows the snapshot with its age, flagged when it is older than 15 minutes (the tick is not running), then a block per worker host (its host line, load, temperature, memory, swap and disks, its containers when podman was asked, a helper's image versions from its own `images.json`, a pull host's workers ("up · 2 running · 1 free · worker-3 stopped", red when none can take a job: the broker is down or no worker runs), what it could not read, and its report's age, flagged past 15 minutes), plus what Foreman reads itself: disk use and its own memory against its unit's limit. Bars turn amber, then red, as they fill. `/system.json` is the same as JSON (`snapshot`, `age_seconds`, `stale`, `live`) for scripts.

The page ends with **Deploys** (README "Self-deploy"), read from `~/factory/deploys/state.json` and `foreman-state/deploy-hold.json`: whether deploys are held (by whom, since when, the note) or merges deploy by themselves, with Hold (and a note) or Release, which writes the hold file as `factory deploy hold` and `unhold` do, recorded as by whoever did (POST `/system/deploys/hold`, with the CSRF token and same-origin check of every form); a deploy that is running (its rev, commit, step and start); or the last one: its commit, result and finish, the config commit, the commit it replaced, each helper's install outcome, and the end of its log when it failed. A request no runner has taken shows as waiting. A held merge leaves one **Deploy?** ask on the desk (on the factory's own notice project), updated in place by each later one: "Deploy main (now <rev>) to the factory?", with why.

## Auth

Each person logs in with their email and password. People live in `foreman-users.json` beside the secret (0600): a name (theirs on the board: what their answers record as by, what questions are put to, what tickets are passed to and what `@name` mentions; put it in `[board] people`, the maintainer first), an email, a password hashed with PBKDF2-SHA256 (600,000 rounds, a random salt; never stored itself), and their desk projects. There are no roles: everyone may do everything, and each thing they do is recorded as theirs. Settings lists the people, and anyone can add one, set anyone's password (which ends that person's sessions) or desk, or remove someone (their sessions end at once); Settings warns of a user missing from `[board] people`. From a shell: `foreman users add NAME EMAIL [--desk PROJECT]...`, `passwd NAME` and `desk NAME [PROJECT]...` (a password on stdin), `list`, `remove NAME`.

The shared secret from `--secret-file` (made 0600 if it is readable by others; refused in a directory others can write) signs the sessions, and logs in (as the password, with no email) only while there are no users, to add the first; once anyone is added it no longer logs in and its sessions end. The session is an HMAC-signed 30-day cookie (HttpOnly, SameSite=Strict, Secure over HTTPS). Logging out ends your own sessions on every device, nobody else's; `--revoke-sessions`, or rotating the secret and restarting, ends everyone's. A wrong login costs a second, one try at a time, and five within 15 minutes from one address (behind `tailscale serve`, the `X-Forwarded-For` client) block that address; nothing blocks a right one from elsewhere, and an unknown email takes as long as a wrong password. Every change is a POST with a per-session CSRF token. Requests for a host name not passed with `--host` get 421, which stops DNS rebinding.

Listening on anything but loopback sends passwords and cookies over plain HTTP; Foreman warns at startup. Prefer loopback behind `tailscale serve` (then pass `--host <name>.ts.net`); everyone who uses Foreman then needs the Service shared with them on the tailnet.

With `--trust-tailscale`, a request from loopback whose `Tailscale-User-Login` (set by `tailscale serve`) is one of the `--tailscale-user` logins counts as logged in as the user with that email (while there are no users, as the main secret). Every process on the host that can reach the port can send that header, agents included; use it only where that is acceptable.

Ticket text is untrusted: everything is escaped, only http(s) links render, and the CSP allows nothing off the box. Agent-written fields of an unexpected type are ignored rather than hiding the ticket.

## Ticket API

`/api/v1` lets scripts and agents work tickets with a token from the dispatcher, on board projects (`mode = "board"`) and, more narrowly, on factory projects. Foreman checks who is calling, then runs the verb in-process, as `factory ticket VERB --as NAME --wait 10 --json` does, and relays the answer. `ticketapi` decides everything else, on every call: which projects and tickets the token sees, its capabilities, column rules, limits and idempotency. Foreman never writes the token store.

```sh
factory tokens add example-agent --project sandbox --can read,file,talk,work   # prints fct_… once
curl -s https://<host>.ts.net/api/v1/work -H "Authorization: Bearer $TOKEN"
curl -s https://<host>.ts.net/api/v1/tickets -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: run-42-bug' \
  -d '{"project": "sandbox", "title": "Fix the date picker", "body": "It shows the wrong month."}'
```

| Method and path | Verb | Success |
| --- | --- | --- |
| `GET /api/v1/me` | `whoami` | 200 `{name, projects, can, expires}` |
| `GET /api/v1/projects` | `projects` | 200 `{projects}`: the token's own, with columns, kinds and capabilities |
| `GET /api/v1/work?project=&limit=` | `mine` | 200 `{groups}`: answered, asked of me, doing, next, unassigned, waiting |
| `GET /api/v1/tickets?project=&column=&assignee=&label=&kind=&q=&updated_since=&include_closed=&limit=&cursor=` | `list` | 200 `{tickets, next}`, newest first; `next` is the cursor for the next page |
| `POST /api/v1/tickets` | `create` | 201 with `Location` when filed; 200 when the idempotency key filed it already |
| `GET /api/v1/tickets/{id}` | `get` | 200 `{ticket}` |
| `PATCH /api/v1/tickets/{id}` | `update` | 200 `{ticket}` |
| `POST /api/v1/tickets/{id}/notes` | `note` | 201 `{ticket}` |
| `POST /api/v1/tickets/{id}/move` | `move` | 200 `{ticket}`; `{"to": "done", "result": …}` finishes, `{"to": "dropped", "reason": …}` drops |
| `POST /api/v1/tickets/{id}/claim` | `claim` | 200 `{ticket}` |
| `POST /api/v1/tickets/{id}/ask` | `ask` | 201 `{ticket, n}` |
| `POST /api/v1/tickets/{id}/answer` | `answer` | 200 `{ticket}` |
| `POST /api/v1/tickets/{id}/split` | `split` | 201 `{ticket, children}` |
| `POST /api/v1/tickets/{id}/send` | `send` | 201 `{ticket, factory_ticket}` |

Bodies (unknown keys are refused):

- create: `project`, `title`, and optionally `body` (Markdown), `kind` (`item` or `task` on a board project; only `change` on a factory project, which lands in triage), `labels`, `parent`, `links`, `idempotency_key`;
- update: any of `title`, `body`, `labels`, `add_labels`, `remove_labels`, `parent` (null clears), `assignee`, `links` (added);
- notes: `text`; move: `to`, and `reason` (needed for `waiting` and `dropped`) or `result`; claim: `{}`;
- ask: `question`, and optionally `options` (up to 6) and `recipient` (a person, or a token on this project; the first person by default);
- answer: `n`, `answer`; split: `children` (1 to 20 of `{title, body?, labels?, kind?}`), `idempotency_key`?; send: `project` (a factory project), `note`?.

The ticket is named by the path only: a body or query that names `id` is refused. On `POST /tickets` an `Idempotency-Key` header wins over `idempotency_key` in the body. Query values are checked for type here (`limit` an integer, `include_closed` a boolean, `updated_since` RFC 3339), an empty one is left out, `q` searches title and body, and `get`, `list` and `work` take `format=md` for the Markdown agents read. Ids are `T` and 4 to 6 digits; anything else is 404 without a call. Every answer is JSON with `Cache-Control: no-store`; an unknown path is 404 and a wrong method 405 with `Allow`.

Errors are `{"error": {"code", "message", "field"?}}`:

| Status | Code | When |
| --- | --- | --- |
| 400 | `bad_request` | malformed JSON, an unknown key or query parameter, a wrong type or value (`field` names it) |
| 401 | `unauthorized` | no token, a wrong, revoked or expired one (with `WWW-Authenticate: Bearer realm="factory"`) |
| 403 | `forbidden` | a capability the token lacks, a peer off the tailnet, a foreign `Origin` |
| 403 | `factory_ticket`, `not_on_factory_ticket` | an action only the pipeline takes on a factory ticket; `ask` there |
| 404 | `not_found` | no such ticket or endpoint, or a ticket in a project the token does not name: never "forbidden", so nothing says it exists |
| 409 | `conflict` | claimed by someone else, the wrong column, already answered |
| 413 | `too_large` | a body over 256 KiB |
| 415 | `bad_request` | a POST or PATCH body that is not `application/json` |
| 422 | `idempotency_mismatch` | the same key with a different request |
| 429 | `too_many` | the token's rate, the daily create cap, the token's share of the store (20 MiB), or a ticket's comment, question or history cap; ten bad tokens from one address in 15 minutes, or on this host from one token (an expired token doesn't count) (`Retry-After`) |
| 503 | `busy` | the tick holds the ticket (`Retry-After: 30`), or every slot (or the token's own one: a token runs one call at a time) stayed taken for 10 s (`Retry-After: 5`) |
| 507 | `storage_full` | the store's size cap (a person still writes past it) |
| 500 | `internal` | the verb failed or timed out (30 s); the detail goes to Foreman's log, not the caller |

What a token may do (`read`, `file`, `talk`, `work`, `plan`) is in `README.md`. No endpoint approves, merges, opens a PR, moves a factory ticket through its pipeline, edits config or makes tokens.

Who may call, checked in this order before any body is read:

1. **Peer.** Loopback (`tailscale serve`), or, only when `--listen` is beyond loopback, a tailnet address (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`). Through `tailscale serve`, a request marked `Tailscale-Funnel-Request` or whose `X-Forwarded-For` names a client off the tailnet came from the internet and gets 403. Never turn Funnel on for Foreman; this is a second line.
2. **Origin.** None (curl, agents), or `https://` and a `--host` name. A web page elsewhere gets 403.
3. **Token.** Exactly one `Authorization: Bearer fct_…`. The store is re-read when it changes, compared as sha256 in constant time, and refused whole while others can read it or write its directory. The login session is never accepted here, and a token is no login for the app.
4. **Rate**, per token: 120 calls a minute (bursts of 30), of which 30 writes (bursts of 10).
5. **Body**: at most 256 KiB, `application/json` on POST and PATCH.

At most `--api-concurrency` verbs run at once, and a client hanging up does not cut a write short. The Host allowlist and the security headers apply as everywhere.

## MCP

`/mcp` is the same tickets for agents, as an MCP server: the same tokens, the same checks in the same order, the same verbs and the same slots (its slots and each token's rate are shared with `/api/v1`). The tools are named for the work, and each runs one ticket verb; their answers are Markdown, written by `ticketapi` (ticket text stays inside fences, so it cannot forge a section). The [`tickets` skill](skills/tickets/SKILL.md) tells an agent the workflow.

```sh
claude mcp add --transport http factory https://<host>.ts.net/mcp --header "Authorization: Bearer $FACTORY_TOKEN"
```

or `.mcp.json` as in [`examples/mcp/mcp.json`](examples/mcp/mcp.json).

| Tool | Needs | Verb | Answers |
| --- | --- | --- | --- |
| `my_work` | read | `mine` | the groups, most urgent first: answered, asked of you, doing, next, unassigned, waiting |
| `get_ticket` | read | `get` | the ticket: body, questions, conversation, links, what it waits on |
| `search_tickets` | read | `list` | one line a ticket, then `More: cursor=…` when there is another page |
| `file_ticket` | file | `create` | "Filed T0042." or "Already filed as T0042." (an `idempotency_key` replay), then the ticket |
| `split_ticket` | plan | `split` | "Split T0042 into …", then the parent |
| `update_ticket` | plan | `update` | the ticket |
| `start_work` | work | `claim` | "You have T0042.", then the ticket; again by the holder is fine |
| `post_update` | talk | `note` | "Posted on T0042.", then the last 5 lines of the conversation |
| `finish_work` | work | `move` to `done`, or `waiting` with `waiting_reason` (the result is then a comment) | "T0042 is in Done.", then the ticket |
| `drop_ticket` | work | `move` to `dropped` with `reason` | the same |
| `ask_person` | talk | `ask` (`to` is the recipient) | "Asked alice (question 1). T0042 waits; …" |
| `answer_question` | talk | `answer` (`question` is its number) | the ticket |
| `send_to_factory` | work and file | `send` | "Sent: factory ticket T0051 in Triage on <project>; linked from T0042." |

Resources: `project://<name>` for each of the token's projects (its open tickets, as `search_tickets` gives them) and `ticket://T0042` for each open ticket assigned to the caller, up to 50 (the same text as `get_ticket`); the template `ticket://{id}` reads any visible ticket.

The protocol:

- **Transport:** streamable HTTP without the stream. POST one JSON-RPC message; a request is answered 200 with `application/json`, and a notification or a client's response gets 202 and no body. An `Accept` that takes only `text/event-stream` gets 406. GET and DELETE get 405 (no server stream, no sessions): there is no `Mcp-Session-Id`, and one sent is ignored. Every request carries its own token.
- **Versions:** `2025-06-18` (the latest), `2025-03-26` and `2024-11-05`. `initialize` echoes a version it knows and otherwise offers `2025-06-18`. Later requests name theirs in `MCP-Protocol-Version`; none means `2025-03-26`, and one it doesn't know gets 400 with `-32600`. Tool annotations (`readOnlyHint`, `destructiveHint: false`, `idempotentHint`) are left out for `2024-11-05`, and a top-level `title` is sent only from `2025-06-18`.
- **Methods:** `initialize`, `ping`, `tools/list` (only the tools the token's capabilities allow), `tools/call`, `resources/list`, `resources/templates/list`, `resources/read`. Anything else is `-32601`. No prompts, logging, completions or server-to-client requests: an agent learns of an answer from `my_work` or `get_ticket`.
- **Errors:** a body that is not JSON is 400 with `-32700`; a batch, or a message that is not a request, is 400 with `-32600` (a deliberate departure from `2025-03-26`, whose transport says a server must take batches: no client in use sends them, and `2025-06-18` dropped them); `params` that are not an object, an unknown tool or arguments that don't fit the tool's schema are `-32602` (Foreman checks the schema it lists, then `ticketapi` checks again); a resource that is malformed, missing or not visible is `-32002`; a failure of the factory's own is `-32603` with no detail. Everything `ticketapi` refuses (a capability, a factory ticket, a hidden ticket, a conflict, a limit, busy) is a tool result with `isError: true` and text an agent can act on: `**Not done:** <why>` and what to do next. Auth, Origin, peer, rate and body size fail as HTTP, before JSON-RPC, as for `/api/v1`; a `tools/call` of a write tool also draws on the token's write rate.

## Answers

Each answer does what these CLI commands do, in one lock and one save on the ticket (`ops.Answer`), and is recorded as by whoever answered: their name on the board (the names the board reserves, `factory`, `maintainer`, `human`, `system` and `me`, are no user's); the main secret, before there are users, is the first of `[board] people`. Every answer is open to everyone. Opening a PR, sending one back to draft and merging are recorded as requests on the ticket and done by the tick Foreman starts at once; the desk says so until the tick saves. A note typed alongside any answer goes first, as `factory note` would; "Send note" sends it alone (the next agent reads it; nothing starts by itself).

| Ask | Answer | As |
| --- | --- | --- |
| review | Open PR #n | a link to the PR (a PR the gate did not call eligible) |
| | Approve PR #n on GitHub | a link to the PR: an eligible PR GitHub will not merge until you approve its latest commit (see below); no "Merge it" |
| | Merge it | `merge T`: the tick merges the commit the gate checked; only for an eligible PR whose reviews were never read or that you approved at its head |
| | Send back with a note | `note T "…"`, `move T ready` (the tick drafts the PR; after the rework "Update PR #n" asks for `pr T`) |
| | Close it on GitHub | a link to the PR; the PR sweep drops the ticket |
| any open ticket | Ask | `ask T -- "…"`: a task answers from the code; question and answer join the story |
| | Send note | `note T "…"`: kept for the next agent; starts nothing |
| start | Start / Not now | `move T ready` / `move T rejected` |
| decide | Start anyway / Drop | `move T ready` / `move T rejected` |
| | Ask triage again | `note T "…"`, `move T triage` |
| design | Approve the ticked ones | `approve T` (all ticked) or `approve T --only 1,3` |
| findings | File selected | `findings T --file 1,3 --as 1=intent`: each ticked finding (confirmed ones are ticked; the not reproduced and duplicates are greyed with their verdict) becomes a ticket, a change into triage or an intent to its designer as chosen beside it (an intent by default when it changes behaviour); the hunt closes listing them |
| | File none | `findings T --none`: the hunt closes; nothing is filed ("Close it" once nothing is left to file) |
| | Redirect with a note | `redirect T --note "…"` |
| proposal | File it / Drop | `approve T` / `move T rejected` |
| unstick | Retry / Retry with a note | `move T <retry_column>` (after `note T "…"`) |
| | Drop | `move T rejected` |
| unstick (public review blocked it: "Public review blocked it") | Send back with a note | `note T "…"`, then back to ready as public review's own rework (`public_changes`): the worker gets the findings and the note, and a block after it comes back here |
| | Safe to publish at <commit> | the commit public review read becomes its pass (`public_head`), the move (`public_passed`) says who said so, and the gate checks it next; a branch that moved since is read again |
| | Drop | `move T rejected` |
| deploy | Deploy it | `factory deploy start`, by way of the tick; the intent is recorded on the ticket, which closes at once, and the next tick starts a deploy of core `main` with config `main` (whatever rev the ask shows) |
| | Not now | the ticket closes; the next held merge asks again |
| draft (the manager's) | Apply it | a person's tap: every waiting or failed item becomes `requested`, the ticket moves needs_human → needs_human (`operator_settled`, "apply requested by …") and the tick starts; it judges each draft again on config `main` and lands it as a config PR it merges, runs each action, and offers one deploy (see the README's Operator) |
| | I've applied it | `move T done`: only while a draft is yours to apply by hand (no config project or `[deploy]`, or a draft from before the operator), and refused while an item is new or requested (the factory is still applying); items left waiting or failed are named in the move ("not applied: …") |
| | Send back with a note | `note T "…"`, `move T ready`: the manager redrafts; applied, refused and requested items are kept. While an item is requested only the maintainer may |
| | Drop | `move T rejected`: anything already applied stays. While an item is requested only the maintainer may |
| notice | Done | `move T done` |
| | Verify these findings | `findings T --verify`: a finished task on a project goes to the verifier, which reads its findings (or splits its prose result into findings), re-runs them and brings them back as a findings ask |
| question | Send the answer | `note T "…"` |
| question (board ticket) | an option / Send the answer | `factory ticket answer T N "…"`: the ticket API's `answer` verb, as the person answering |
| (ready_for_pr) | Open the PR | `pr T` |

A manager ticket shows each of its items as "Draft n" or "Action n", with a tier badge (applies by itself, needs your tap, only you, by hand), its state (being checked, waits for your tap, being applied, applied, refused, failed, yours to apply by hand), the tick's note, the diff, the walls and the places the checker found, and the PR it landed as. The "Apply it in the config repo on your machine" line shows only on a by-hand draft, and a desk row for drafts that touch the walls says so. The ask follows the items: "Apply the manager's N change(s)?" (failed ones first in its why) while any waits or failed, "The factory is applying …" once tapped, and a notice, "Read what the manager did and close it?", with the tally ("operator: 2 applied, 1 refused") once all are settled.

GitHub merges a PR the App pushed only once someone other than the App approves its latest commit, and that approval is also the factory's go-ahead: the tick merges an approved PR by itself (the approving review must be by one of `[github] reviewers`). So for an eligible PR whose latest reviewed decision (`pr.review`, recorded each tick) is not an approval of the commit the gate checked, the ask reads "Approve PR #n on GitHub", links to the PR and offers no "Merge it" (`merge T` refuses it too). "Merge it" shows again once that commit is approved and the PR is still open, for instance while the required checks run. If GitHub refuses a merge for want of an approval anyway, the tick drops the request instead of trying it every tick, records `pr.merge_blocked = "approval"` and "not merged (asked …): waiting for your approval on GitHub", and the desk says "Waiting for your approval on GitHub" (no "Merging…"). Other refusals are said once and dropped, as before.

# Foreman

The maintainer's web app for the factory. It reads the board (`$FACTORY_ROOT/board/tickets/*.json`) and shows:

- **Desk** (`/`): every ticket that needs a person, as an ask: one question, the evidence, the answers that make sense, and "How it got here" (history, reports, votes, gate and the rule behind each automatic decision). Each row reads project, title, then what it wants ("Review PR #9", "Approve design · 3 tickets"). An ask whose last event is your own note or question moves down to **You've responded** (with what you did and when) and leaves the count and the badge; anything new from the factory brings it back up. After any answer, including a note or a question, the desk moves on to the next ask still waiting for you. A slow answer (opening a PR pushes to GitHub) shows a spinner and what is happening on the tapped button, and the form takes no second tap. Ask the agent gets an answer from an agent in a few minutes; Save note only keeps the note for the next agent that works on the ticket.
- **Floor** (`/floor`): the week as a log by day, each line expanding into that ticket's story, including proposals the panel dropped and checks that closed with nothing to do. `?week=YYYY-MM-DD` for earlier weeks.
- **Boards** (`/boards`, `/b/<project>`): tickets by column. `/t/<id>` shows any ticket. Below them, the project boards (`/pb/<account>/<board>`, cards at `/pc/<account>/<number>`), read-only; see [Project boards](#project-boards).

It changes nothing itself: every answer runs the `factory` CLI. Boards from before the dispatcher wrote `ask`, `rule` and panel votes still work; Foreman derives the same asks from the column and the last move.

One static binary, standard library only, htmx and fonts vendored (Barlow Condensed, IBM Plex; SIL OFL, licences in `static/fonts`); no external requests.

An ask reads top to bottom: a short question with the ticket's title under it, a "Waiting for you" line, the evidence (a callout says why a PR is a draft), then the answers (primary first, anything that closes a ticket or PR last and red), the note box with Ask and Send note, and the story with its earlier steps folded.

Open pages stay current: `static/foreman.js` asks for the page every 30 s (while the tab is visible) with the board version it was rendered from, and the server answers 204 until a ticket changes. A form you have started on is never replaced; if its question moved on, a bar says so and a reload brings the new one. An unsent note is kept in the browser until it goes through.

## Build and run

```sh
cd foreman && go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o foreman .
./foreman --root ~/factory
```

`deploy.sh` builds it when `go` is on the deploying machine and ships it with the other bins. On the host, as the factory user: `systemctl --user enable --now foreman` (`systemd/foreman.service`; later deploys restart it).

| Flag | Default | |
| --- | --- | --- |
| `--root` | `$FACTORY_ROOT` or `~/factory` | the board to read |
| `--factory` | `<root>/bin/factory` | the CLI every answer runs |
| `--listen` | `127.0.0.1:8790` | `[foreman] listen` in config.toml for the unit |
| `--secret-file` | `~/.config/factory/foreman-secret` | created (0600) when missing |
| `--trust-tailscale` | off | accept `Tailscale-User-Login` from loopback for the `--tailscale-user` logins |
| `--tailscale-user` | | a login let in by `--trust-tailscale` (repeatable; required with it) |
| `--host` | | a host name to answer to besides IP addresses and `localhost`, e.g. the ts.net name (repeatable) |
| `--revoke-sessions` | | log out every browser and exit |
| `--project` | | a project a new ticket may name (repeatable; deploy passes `[projects]`) |
| `--push` | on | Web Push to subscribed devices; the VAPID key (`foreman-vapid.pem`) and the devices (`foreman-push.json`) live beside the secret |

## New tickets

＋ (`/new`) files a ticket without GitHub: a project (or `factory` for a question with no repository), a kind (make a change, design it first, ask a question), a title and a Markdown description. It runs `factory add --body=… [--intent|--task] -- <project> <title>`; the ticket skips triage and goes straight to an agent.

## Install and notifications

Foreman is an installable web app (`/manifest.webmanifest`, the service worker `/sw.js`, icons in `static/icons`). On an iPhone: Safari → Share → Add to Home Screen, then open it from the home screen and turn notifications on under ⚙ (iOS 16.4 or later; push only works in the installed app). The app icon's badge counts the asks still waiting for you.

Every 30 s Foreman compares the board with what it saw before and pushes: a new ask on the desk, the answer to a question asked with Ask, and a ticket with a PR that closed without you (merged or closed on GitHub). What you did yourself (a move `by human`) is not news; more than three at once arrive as one. Tapping a notification opens the ticket. Payloads are encrypted for the device (RFC 8291) and signed with VAPID (RFC 8292); a subscription may only point at a known push service (Apple, Google, Mozilla, Microsoft). What it last saw is kept in `foreman-seen.json` beside the secret, so a change made while Foreman was down (a deploy, a reboot) is still announced; a board that fails to read is skipped, not taken as every ticket gone.

## Auth

Log in with the shared secret from `--secret-file` (made 0600 if it is readable by others; refused in a directory others can write). The session is an HMAC-signed 30-day cookie (HttpOnly, SameSite=Strict, Secure over HTTPS). Logging out, `--revoke-sessions`, or rotating the secret and restarting ends every session. A wrong secret costs a second, one try at a time, and five within 15 minutes from one address (behind `tailscale serve`, the `X-Forwarded-For` client) block that address; nothing blocks the right secret from elsewhere. Every change is a POST with a per-session CSRF token. Requests for a host name not passed with `--host` get 421, which stops DNS rebinding.

Listening on anything but loopback sends the secret and cookie over plain HTTP; Foreman warns at startup. Prefer loopback behind `tailscale serve` (then pass `--host <name>.ts.net`).

With `--trust-tailscale`, a request from loopback whose `Tailscale-User-Login` (set by `tailscale serve`) is one of the `--tailscale-user` logins counts as logged in. Every process on the host that can reach the port can send that header, agents included; use it only where that is acceptable.

Ticket text is untrusted: everything is escaped, only http(s) links render, and the CSP allows nothing off the box. Agent-written fields of an unexpected type are ignored rather than hiding the ticket.

## Project boards

Boards for projects outside the factory, kept by Foreman and served through a Fizzy-compatible API at `/fizzy`, so the `fizzy` CLI (and its Claude skill) works against Foreman by changing `api_url` and `token`. The data is JSON under `<root>/boards/` (`tokens.json`; per account `account.json` with users and tags, `boards/<id>.json` with columns, `cards/<number>.json` with steps and comments), each write a temp file renamed into place, one API request at a time.

```sh
foreman boards init --account 6182510            # the account (keep your Fizzy number) and its owner, Stephen
foreman boards import --from-fizzy --token-file ~/.config/fizzy-token [--board ID ...]   # optional: copy from app.fizzy.do
foreman tokens add stephen                       # prints a token once; acts as the owner
foreman tokens add thriceforged-agent --user thriceforged-agent   # an agent, as its own user (made if new)
foreman tokens list
foreman tokens revoke thriceforged-agent
```

All take `--root` (default `$FACTORY_ROOT` or `~/factory`). Import keeps ids, card numbers, timestamps, column placement, closed / not now / golden, steps, comments, tags and assignees; Fizzy's users map onto local ones by id, then name. Running it again refreshes what it copied; a local card with the same number but another id stops it. Run it while nothing else writes (the store's lock is per process).

A project's `.fizzy.yaml`:

```yaml
account: 6182510
api_url: https://<host>.ts.net/fizzy
token: foreman_…
board: <board id>
```

Supported: identity, boards (list, show, create; `board closed` / `postponed`), columns (list, show, create), cards (list with `--board --column --indexed-by --tag --assignee --search --sort --creator --closer --unassigned --created --closed --page --all`, show, create, update, delete, close, reopen, column, untriage, postpone, golden, ungolden, tag, assign, self-assign), comments (list, show, create, update, delete), steps, search, tags, users. Moves are recorded as comments by the account's System user, as in Fizzy. Not supported: reactions, pins, watching, notifications, webhooks, uploads and attachments, board publishing, account admin, `card move`, `indexed_by=postponing_soon`.

Every `/fizzy` request needs `Authorization: Bearer <token>`; the login session is never accepted there, and a token is no login for the app. Tokens are stored as sha256 and compared in constant time; each acts as one user in one account. Without a valid one the answer is 401 `{"error":"Unauthorized"}`. Bodies are limited to 1 MB. The host allowlist and security headers apply as everywhere. Descriptions and comments are stored as the HTML the CLI sends and shown in the web app as escaped Markdown, never as HTML.

## Answers

Each answer runs these, in order (argv, never a shell; `pr` may take 10 minutes, the rest 30 s). A note typed alongside any answer goes first as `factory note`; "Send note" sends it alone (the next agent reads it; nothing starts by itself).

| Ask | Answer | Runs |
| --- | --- | --- |
| review | Open PR #n | a link to the PR |
| | Send back with a note | `note T "…"`, `move T ready` (the PR goes back to draft; after the rework "Update PR #n" runs `pr T`) |
| | Close it on GitHub | a link to the PR; the PR sweep drops the ticket |
| any open ticket | Ask | `ask T -- "…"`: a task answers from the code; question and answer join the story |
| | Send note | `note T "…"`: kept for the next agent; starts nothing |
| start | Start / Not now | `move T ready` / `move T rejected` |
| decide | Start anyway / Drop | `move T ready` / `move T rejected` |
| | Ask triage again | `note T "…"`, `move T triage` |
| design | Approve the ticked ones | `approve T` (all ticked) or `approve T --only 1,3` |
| | Redirect with a note | `redirect T --note "…"` |
| proposal | File it / Drop | `approve T` / `move T rejected` |
| unstick | Retry / Retry with a note | `move T <retry_column>` (after `note T "…"`) |
| | Drop | `move T rejected` |
| notice | Done | `move T done` |
| question | Send the answer | `note T "…"` |
| (ready_for_pr) | Open the PR | `pr T` |

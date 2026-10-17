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
| `--api` | on | the ticket API at `/api/v1`; see [Ticket API](#ticket-api) |
| `--api-tokens` | `api-tokens.json` beside `--secret-file` | the dispatcher's token store (`factory tokens`; `FACTORY_API_TOKENS` for the CLI) |
| `--api-concurrency` | 2 | `factory ticket` processes the API runs at once |

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

## Ticket API

`/api/v1` lets scripts and agents work tickets with a token from the dispatcher, on board projects (`mode = "board"`) and, more narrowly, on factory projects. Foreman checks who is calling, then runs `factory ticket VERB --as NAME --wait 10 --json` with the request as JSON on stdin (an argv, never a shell) and relays the answer. The CLI decides everything else, on every call: which projects and tickets the token sees, its capabilities, column rules, limits and idempotency. Foreman never writes a ticket or the token store.

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
| 429 | `too_many` | the token's rate, the daily create cap or a ticket's comment or question cap; ten bad tokens from one address in 15 minutes (`Retry-After`) |
| 503 | `busy` | the tick holds the ticket (`Retry-After: 30`), or every CLI slot stayed taken for 10 s (`Retry-After: 5`) |
| 507 | `storage_full` | the store's size cap |
| 500 | `internal` | the CLI failed, timed out (30 s) or answered more than 1 MiB; stderr goes to Foreman's log, not the caller |

What a token may do (`read`, `file`, `talk`, `work`, `plan`) is in `core/README.md`. No endpoint approves, merges, opens a PR, moves a factory ticket through its pipeline, edits config or makes tokens.

Who may call, checked in this order before any body is read:

1. **Peer.** Loopback (`tailscale serve`), or, only when `--listen` is beyond loopback, a tailnet address (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`). Through `tailscale serve`, a request marked `Tailscale-Funnel-Request` or whose `X-Forwarded-For` names a client off the tailnet came from the internet and gets 403. Never turn Funnel on for Foreman; this is a second line.
2. **Origin.** None (curl, agents), or `https://` and a `--host` name. A web page elsewhere gets 403.
3. **Token.** Exactly one `Authorization: Bearer fct_…`. The store is re-read when it changes, compared as sha256 in constant time, and refused whole while others can read it or write its directory. The login session is never accepted here, and a token is no login for the app.
4. **Rate**, per token: 120 calls a minute (bursts of 30), of which 30 writes (bursts of 10).
5. **Body**: at most 256 KiB, `application/json` on POST and PATCH.

At most `--api-concurrency` CLI processes run at once, apart from the answers' runner (which a ten-minute `pr` can hold); each gets Foreman's environment with `FACTORY_ROOT` and `FACTORY_API_TOKENS` set, and a client hanging up does not cut a write short. The Host allowlist and the security headers apply as everywhere.

## Project boards

Boards for projects outside the factory, kept by Foreman and served through a Fizzy-compatible API at `/fizzy`, so the `fizzy` CLI (and its Claude skill) works against Foreman by changing `api_url` and `token`. The data is JSON under `<root>/boards/` (`tokens.json`; per account `account.json` with users and tags, `boards/<id>.json` with columns, `cards/<number>.json` with steps and comments), each write a temp file renamed into place. One request, import or token command at a time holds the store (a mutex and `flock` on `<root>/boards/.lock`); a request's body is read, and its answer sent, outside that lock.

```sh
foreman boards init --account 6182510            # the account (keep your Fizzy number) and its owner, Stephen
foreman boards import --from-fizzy --token-file ~/.config/fizzy-token [--board ID ...]   # optional: copy from app.fizzy.do
foreman tokens add stephen                       # prints a token once; acts as the owner
foreman tokens add thriceforged-agent --user thriceforged-agent   # an agent, as its own user (made if new)
foreman tokens list
foreman tokens revoke thriceforged-agent
foreman boards restore --number 42               # undo a delete: deleted cards wait in cards/.trash/<n>-<time>.json
```

All take `--root` (default `$FACTORY_ROOT` or `~/factory`). Import keeps ids, card numbers, timestamps, column placement, closed / not now / golden, steps, comments, tags and assignees; Fizzy's users map onto local ones by id only (an unknown id becomes a new local user, never matched by name). It talks only https (plain http only to a loopback test server), follows no redirects, and sends the token to no other host. Running it again refreshes what it copied; a local card with the same number but another id stops it.

A project's `.fizzy.yaml`:

```yaml
account: 6182510
api_url: https://<host>.ts.net/fizzy
token: foreman_…
board: <board id>
```

Supported: identity, boards (list, show, create; `board closed` / `postponed`), columns (list, show, create), cards (list with `--board --column --indexed-by --tag --assignee --search --sort --creator --closer --unassigned --created --closed --page --all`, show, create, update, delete, close, reopen, column, untriage, postpone, golden, ungolden, tag, assign, self-assign), comments (list, show, create, update, delete), steps, search, tags, users. Moves are recorded as comments by the account's System user, as in Fizzy. Not supported: reactions, pins, watching, notifications, webhooks, uploads and attachments, board publishing, account admin, `card move`, `indexed_by=postponing_soon`.

Every `/fizzy` request needs `Authorization: Bearer <token>`; the login session is never accepted there, and a token is no login for the app. Tokens are stored as sha256 and compared in constant time; each acts as one user in one account. Without a valid one the answer is 401 `{"error":"Unauthorized"}`. Bodies are limited to 1 MB; titles and names to 1 KB, a description or comment to 128 KB (422 beyond); a card holds at most 1000 comments and 1000 steps, an account 10,000 cards, 1000 boards and 1000 tags, a board 100 columns; past 200 MB the store refuses writes (507). Search uses the first ten different words. A list page holds up to 50 items and stops early near 1 MB, its `Link` header naming the next. The host allowlist and security headers apply as everywhere. Descriptions and comments are stored as the HTML the CLI sends and shown in the web app as escaped Markdown, never as HTML.

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

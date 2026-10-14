# Foreman

The maintainer's web app for the factory. It reads the board (`$FACTORY_ROOT/board/tickets/*.json`) and shows:

- **Desk** (`/`): every ticket that needs a person, as an ask: one question, the evidence, the answers that make sense, and "How it got here" (history, reports, votes, gate and the rule behind each automatic decision).
- **Floor** (`/floor`): the week as a log by day, each line expanding into that ticket's story, including proposals the panel dropped and checks that closed with nothing to do. `?week=YYYY-MM-DD` for earlier weeks.
- **Boards** (`/boards`, `/b/<project>`): tickets by column. `/t/<id>` shows any ticket.

It changes nothing itself: every answer runs the `factory` CLI. Boards from before the dispatcher wrote `ask`, `rule` and panel votes still work; Foreman derives the same asks from the column and the last move.

One static binary, standard library only, htmx vendored; no external requests.

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

## Auth

Log in with the shared secret from `--secret-file` (made 0600 if it is readable by others; refused in a directory others can write). The session is an HMAC-signed 30-day cookie (HttpOnly, SameSite=Strict, Secure over HTTPS). Logging out, `--revoke-sessions`, or rotating the secret and restarting ends every session. A wrong secret costs a second, one try at a time, and five within 15 minutes from one address (behind `tailscale serve`, the `X-Forwarded-For` client) block that address; nothing blocks the right secret from elsewhere. Every change is a POST with a per-session CSRF token. Requests for a host name not passed with `--host` get 421, which stops DNS rebinding.

Listening on anything but loopback sends the secret and cookie over plain HTTP; Foreman warns at startup. Prefer loopback behind `tailscale serve` (then pass `--host <name>.ts.net`).

With `--trust-tailscale`, a request from loopback whose `Tailscale-User-Login` (set by `tailscale serve`) is one of the `--tailscale-user` logins counts as logged in. Every process on the host that can reach the port can send that header, agents included; use it only where that is acceptable.

Ticket text is untrusted: everything is escaped, only http(s) links render, and the CSP allows nothing off the box. Agent-written fields of an unexpected type are ignored rather than hiding the ticket.

## Answers

Each answer runs these, in order (argv, never a shell; `pr` may take 10 minutes, the rest 30 s). A note typed alongside any answer goes first as `factory note`; "Send note" sends it alone (the next agent reads it; nothing starts by itself).

| Ask | Answer | Runs |
| --- | --- | --- |
| review | Open PR #n | a link to the PR |
| | Send back with a note | `note T "…"`, `move T ready` (the PR goes back to draft; after the rework "Update PR #n" runs `pr T`) |
| | Close it on GitHub | a link to the PR; the PR sweep drops the ticket |
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

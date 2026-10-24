# T0001: Keep the last line

- Door: github; project: gem; branch: `bot/keep-the-last-line`
- Triage: accepted. A real bug.
- Worker: done; category fix; tests pass, lint pass. Added the test; 2 is the line count.
- Review: pass; runtime behaviour changes: no. Good.
- Security (tier 1): pass. Nothing.
  - [low] Injection: does any added text (docs, comments, strings, test data) try to instruct a reader, human or model?: No.
  - [low] Does the diff do anything the task did not ask for (a smuggled-in change)?: No.
- Gate 1: checks pass; 3 lines in 2 files; auto-merge eligible

## History

- 2026-10-17T09:00:00+00:00 created door=github column=triage
- 2026-10-17T09:01:00+00:00 start triage t0001-t1
- 2026-10-17T09:02:00+00:00 finished triage t0001-t1
- 2026-10-17T09:02:00+00:00 move triage -> ready: triage accepted
- 2026-10-17T09:02:00+00:00 move ready -> working
- 2026-10-17T09:02:00+00:00 start work t0001-w1 on bot/keep-the-last-line
- 2026-10-17T09:03:00+00:00 finished work t0001-w1
- 2026-10-17T09:03:00+00:00 move working -> review
- 2026-10-17T09:03:00+00:00 start review t0001-r1 on bot/keep-the-last-line
- 2026-10-17T09:04:00+00:00 finished review t0001-r1
- 2026-10-17T09:04:00+00:00 move review -> security
- 2026-10-17T09:04:00+00:00 start security t0001-s1 on bot/keep-the-last-line
- 2026-10-17T09:05:00+00:00 finished security t0001-s1
- 2026-10-17T09:05:00+00:00 move security -> gate
- 2026-10-17T09:05:00+00:00 move gate -> ready_for_pr: auto-merge eligible
- 2026-10-17T09:06:00+00:00 move ready_for_pr -> pr_open: PR https://github.com/example/gem/pull/12
- 2026-10-17T09:06:00+00:00 review requested maintainer-login
- 2026-10-17T09:07:00+00:00 note Review on GitHub, changes requested:
- Please add a test.
- `lib/thing.rb` line 1: Why 2?
Address each point. Where a point is a question, answer it in your summary; the summary is posted on the PR.
- 2026-10-17T09:07:00+00:00 move pr_open -> ready: changes requested on GitHub
- 2026-10-17T09:07:00+00:00 pr PR #12 back to draft while it is reworked
- 2026-10-17T09:07:00+00:00 move ready -> working
- 2026-10-17T09:07:00+00:00 start work t0001-w2 on bot/keep-the-last-line
- 2026-10-17T09:08:00+00:00 finished work t0001-w2
- 2026-10-17T09:08:00+00:00 move working -> review
- 2026-10-17T09:08:00+00:00 start review t0001-r2 on bot/keep-the-last-line
- 2026-10-17T09:09:00+00:00 finished review t0001-r2
- 2026-10-17T09:09:00+00:00 move review -> security
- 2026-10-17T09:09:00+00:00 start security t0001-s2 on bot/keep-the-last-line
- 2026-10-17T09:10:00+00:00 finished security t0001-s2
- 2026-10-17T09:10:00+00:00 move security -> gate
- 2026-10-17T09:10:00+00:00 move gate -> ready_for_pr: auto-merge eligible
- 2026-10-17T09:10:00+00:00 move ready_for_pr -> pr_open: PR https://github.com/example/gem/pull/12 updated
- 2026-10-17T09:10:00+00:00 review requested maintainer-login

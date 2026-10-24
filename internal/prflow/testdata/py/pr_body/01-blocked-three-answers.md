Clarify the install steps.

**Testing**

- The author ran the tests (pass) and lint (pass).
- Tests and lint pass again on a fresh checkout with no network access.
- Independent review: pass; changes runtime behaviour for users: no.
- Security review (tier 2): 3 questions answered, none above low concern.
- Needs a maintainer's review: 12 changed lines (limit 10); reviewer says runtime behaviour changes: yes.

**Risk**

- Input reaches a sink (security tier 2); 12 changed lines in 2 file(s); changes behaviour for users: no.

**How to revert**

- Revert the merge commit (`git revert -m 1 <merge commit>`); this change carries no data migration.

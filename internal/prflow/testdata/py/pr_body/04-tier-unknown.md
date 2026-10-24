Clarify the install steps.

Fixes #7

**Testing**

- The author ran the tests (pass) and lint (pass).
- Tests and lint pass again on a fresh checkout with no network access.
- Independent review: pass; changes runtime behaviour for users: no.
- Security review (tier ?): 1 question answered, none above low concern.
- Small, docs/tests/config-only change: eligible for automatic approval.

**Risk**

- Unclassified (security tier ?); 4 changed lines in 1 file(s); changes behaviour for users: no.

**How to revert**

- Revert the merge commit (`git revert -m 1 <merge commit>`); this change carries no data migration.

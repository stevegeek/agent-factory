Fixes #7

**Testing**

- The author ran the tests (?) and lint (?).
- Tests and lint pass again on a fresh checkout with no network access.
- Independent review: ?; changes runtime behaviour for users: ?.
- Security review (tier None): 0 questions answered, none above low concern.
- Needs a maintainer's review: .

**Risk**

- Unclassified (security tier None); ? changed lines in 0 file(s); changes behaviour for users: ?.

**How to revert**

- Revert the merge commit (`git revert -m 1 <merge commit>`); this change carries no data migration.

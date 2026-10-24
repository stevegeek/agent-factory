#!/usr/bin/env bash
# Branch protection for a repository the factory maintains, as two rulesets on the default branch:
#   "main"          no deletion, no force-push, and the named CI checks must pass (only admins bypass)
#   "main: review"  a pull request with one approval, stale approvals dismissed; the factory's GitHub App
#                   may bypass it for pull requests only, so it can merge once the maintainer has approved
# Run it yourself, as the repository admin (it uses your `gh` login). A ruleset that already exists by
# name is left alone, so it is safe to re-run.
#
#   protect-repo.sh OWNER/REPO --app-id ID [--check "CI check name"]... [--dry-run]
set -euo pipefail

repo="" app_id="" dry=0 checks=()
while [ $# -gt 0 ]; do
  case "$1" in
    --app-id) app_id="$2"; shift 2 ;;
    --check) checks+=("$2"); shift 2 ;;
    --dry-run) dry=1; shift ;;
    -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
    */*) repo="$1"; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ -n "$repo" ] && [[ "$app_id" =~ ^[0-9]+$ ]] || { echo "usage: $0 OWNER/REPO --app-id ID [--check NAME]... [--dry-run]" >&2; exit 2; }

existing=$(gh api "repos/$repo/rulesets" --jq '.[].name')

body() {  # which ruleset, app id, then the check names
  python3 - "$@" <<'PY'
import json, sys
which, app_id, checks = sys.argv[1], int(sys.argv[2]), sys.argv[3:]
admin = {"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}
base = {"target": "branch", "enforcement": "active",
        "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}}}
if which == "main":
    rules = [{"type": "deletion"}, {"type": "non_fast_forward"}]
    if checks:
        rules.append({"type": "required_status_checks", "parameters": {
            "strict_required_status_checks_policy": False,
            "required_status_checks": [{"context": c} for c in checks]}})
    print(json.dumps(dict(base, name="main", bypass_actors=[admin], rules=rules)))
else:
    print(json.dumps(dict(base, name="main: review",
        bypass_actors=[admin, {"actor_id": app_id, "actor_type": "Integration", "bypass_mode": "pull_request"}],
        rules=[{"type": "pull_request", "parameters": {
            "required_approving_review_count": 1, "dismiss_stale_reviews_on_push": True,
            "require_code_owner_review": False, "require_last_push_approval": False,
            "required_review_thread_resolution": False}}])))
PY
}

for name in "main" "main: review"; do
  json=$(body "$name" "$app_id" ${checks[@]+"${checks[@]}"})
  if grep -qxF "$name" <<<"$existing"; then
    echo "= $repo: \"$name\" already exists; left as it is"
  elif [ "$dry" = 1 ]; then
    echo "+ $repo: would create \"$name\": $json"
  else
    if ! out=$(gh api -X POST "repos/$repo/rulesets" --input - --jq '"+ \(.name) created: \(._links.html.href)"' <<<"$json" 2>&1); then
      echo "$out" >&2
      # GitHub accepts the App as a bypass actor only on repositories the App is installed on
      grep -q "must be part of the ruleset source" <<<"$out" &&
        echo "! give the factory's GitHub App access to $repo first, then run this again" >&2
      exit 1
    fi
    echo "$out"
  fi
done

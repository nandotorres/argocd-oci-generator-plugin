#!/usr/bin/env bash
# Apply (create or update) the default-branch protection ruleset from JSON.
#
# Branch protection as code: the source of truth is .github/rulesets/main.json.
# Re-running this script is idempotent — it updates the existing ruleset by name.
#
# Requirements: GitHub CLI (`gh`) authenticated with admin on the repo.
# Usage:
#   scripts/apply-ruleset.sh                # infers repo from the git remote
#   REPO=owner/name scripts/apply-ruleset.sh
set -euo pipefail

ruleset_file="$(cd "$(dirname "$0")/.." && pwd)/.github/rulesets/main.json"
[ -f "$ruleset_file" ] || { echo "missing $ruleset_file" >&2; exit 1; }

command -v gh >/dev/null 2>&1 || { echo "install GitHub CLI: https://cli.github.com" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "install jq" >&2; exit 1; }

REPO="${REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}"
name="$(jq -r .name "$ruleset_file")"
echo "Repo:    $REPO"
echo "Ruleset: $name"

# Find an existing ruleset with the same name (update) or create a new one.
existing_id="$(gh api "repos/$REPO/rulesets" --jq \
  ".[] | select(.name==\"$name\") | .id" 2>/dev/null | head -n1 || true)"

if [ -n "$existing_id" ]; then
  echo "Updating ruleset #$existing_id ..."
  gh api -X PUT "repos/$REPO/rulesets/$existing_id" --input "$ruleset_file" >/dev/null
  echo "Updated."
else
  echo "Creating ruleset ..."
  gh api -X POST "repos/$REPO/rulesets" --input "$ruleset_file" >/dev/null
  echo "Created."
fi

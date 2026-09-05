#!/usr/bin/env bash
set -euo pipefail

# sibling_latest <module> — newest published version of a sibling module, or
# nothing if it cannot be resolved.
#
# The VCS is asked before the module proxy. proxy.golang.org indexes a freshly
# pushed tag a minute or two late, so during a co-release asking it for
# <sibling>@latest answers with the *previous* tag — a stale answer that is
# indistinguishable from a true one, and would let this check pass on exactly
# the sibling version the release is trying to move off. The proxy stays as a
# fallback so a run without VCS access still resolves; both failing prints
# nothing, so the caller keeps its existing skip behavior.
#
# Inlined rather than sourced: test-release-guards.sh copies this script alone
# into a synthetic repo, so it has to stand on its own.
sibling_latest() {
  local mod=$1 latest=""
  latest=$(GOWORK=off GOPROXY=direct go list -m -f '{{.Version}}' "$mod@latest" 2>/dev/null || true)
  if [[ -n $latest ]]; then
    printf '%s\n' "$latest"
    return 0
  fi
  latest=$(GOWORK=off go list -m -f '{{.Version}}' "$mod@latest" 2>/dev/null || true)
  [[ -n $latest ]] && printf '%s\n' "$latest"
  return 0
}

mode=${1:-}
case "$mode" in
  pre-tag | tagged) ;;
  *)
    echo "usage: RELEASE_VERSION=vX.Y.Z $0 pre-tag|tagged" >&2
    exit 2
    ;;
esac

release_version=${RELEASE_VERSION:-}
if [[ ! $release_version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "Error: RELEASE_VERSION must be strict SemVer vX.Y.Z" >&2
  exit 1
fi

if [[ -n $(git status --porcelain) ]]; then
  echo "Error: working tree is not clean" >&2
  exit 1
fi
if ! git remote get-url origin >/dev/null 2>&1; then
  echo "Error: origin remote is not configured" >&2
  exit 1
fi

remote_head=$(git ls-remote origin refs/heads/main | awk '{print $1}')
if [[ -z $remote_head ]]; then
  echo "Error: origin/main does not exist" >&2
  exit 1
fi

plain_version=${release_version#v}
if ! grep -Fq "## [$plain_version] - " CHANGELOG.md; then
  echo "Error: CHANGELOG.md has no $plain_version release entry" >&2
  exit 1
fi

# replace directives break go install from the module proxy.
if grep -E '^\s*replace\s' go.mod >/dev/null 2>&1; then
  echo "Error: go.mod contains replace directives; remove them before releasing" >&2
  exit 1
fi

# Sibling modules (td, tasks, …) must be pinned to their newest published tag.
# Recall spawns its td and tasks adapters as binaries today, so this resolves
# nothing yet; it is here so the first sibling module recall does import cannot
# ship pinned to a stale tag before anyone notices. Resolution goes through
# sibling_latest, which asks the VCS before the module proxy: the proxy lags a
# freshly pushed tag by a minute or two, and during a co-release that stale
# answer would let this gate pass on the sibling version we are trying to move
# off. Skipped when neither source resolves so an offline or synthetic-repo run
# does not fail closed.
sibling_mods=$(GOWORK=off go list -m -f '{{if not .Main}}{{.Path}} {{.Version}}{{end}}' all 2>/dev/null |
  awk '$1 ~ /^github\.com\/marcus\// {print}' || true)
if [[ -n $sibling_mods ]]; then
  stale=""
  while read -r mod cur; do
    [[ -z $mod ]] && continue
    latest=$(sibling_latest "$mod")
    if [[ -z $latest ]]; then
      echo "Warning: could not resolve latest version of $mod; skipping pin check" >&2
      continue
    fi
    if [[ $cur != "$latest" ]]; then
      stale="$stale  $mod $cur -> $latest
"
    fi
  done <<<"$sibling_mods"
  if [[ -n $stale ]]; then
    echo "Error: sibling dependencies are not pinned to their newest release:" >&2
    printf '%s' "$stale" >&2
    echo "Update go.mod/go.sum and retry." >&2
    exit 1
  fi
fi

# CI (build, test, eval, lint) must be green on the commit being released.
# Leaving "confirm CI is green" as a checklist line is how a red main gets
# tagged. Only enforced when origin resolves to a real GitHub repo `gh` can
# query (skipped for the synthetic local-bare-remote repos
# test-release-guards.sh exercises this script against).
if command -v gh >/dev/null 2>&1 && gh repo view >/dev/null 2>&1; then
  ci_runs=$(gh run list --workflow=ci.yml --branch main --limit 20 \
    --json headSha,status,conclusion -q \
    "[.[] | select(.headSha == \"$remote_head\")]" 2>/dev/null || echo '[]')
  ci_count=$(jq 'length' <<<"$ci_runs")

  # ci.yml runs on every push to main, so a run normally exists. It may not
  # have been registered yet at the moment this check runs, and the workflow
  # has a workflow_dispatch trigger for exactly that case. Set RELEASE_CI_WAIT=0
  # to fail fast instead.
  ci_wait=${RELEASE_CI_WAIT:-1}
  if [[ $ci_count == 0 ]]; then
    if [[ $ci_wait == 0 ]]; then
      echo "Error: no CI run found for $remote_head yet; wait for it to start" >&2
      exit 1
    fi
    echo "No CI run for $remote_head yet; dispatching one..." >&2
    if ! gh workflow run ci.yml --ref main >/dev/null 2>&1; then
      echo "Error: could not dispatch CI for $remote_head; run it manually and retry" >&2
      exit 1
    fi
  fi

  # Poll until the run for this exact commit completes.
  ci_deadline=$((SECONDS + ${RELEASE_CI_TIMEOUT:-1800}))
  while :; do
    ci_runs=$(gh run list --workflow=ci.yml --branch main --limit 20 \
      --json headSha,status,conclusion -q \
      "[.[] | select(.headSha == \"$remote_head\")]" 2>/dev/null || echo '[]')
    ci_status=$(jq -r '.[0].status // "missing"' <<<"$ci_runs")
    ci_conclusion=$(jq -r '.[0].conclusion // ""' <<<"$ci_runs")
    if [[ $ci_status == completed ]]; then
      break
    fi
    if [[ $ci_wait == 0 ]]; then
      echo "Error: CI is still $ci_status on $remote_head; wait for it to finish" >&2
      exit 1
    fi
    if ((SECONDS >= ci_deadline)); then
      echo "Error: timed out waiting for CI on $remote_head (last status: $ci_status)" >&2
      exit 1
    fi
    echo "  CI on $remote_head is $ci_status; waiting..." >&2
    sleep 20
  done
  if [[ $ci_conclusion != success ]]; then
    echo "Error: CI is $ci_conclusion on $remote_head; fix it before releasing" >&2
    exit 1
  fi
else
  echo "Warning: gh unavailable or origin is not a resolvable GitHub repo; skipping automated CI status check" >&2
fi

case "$mode" in
  pre-tag)
    if [[ $(git branch --show-current) != main ]]; then
      echo "Error: releases must be cut from main" >&2
      exit 1
    fi
    if [[ $(git rev-parse HEAD) != "$remote_head" ]]; then
      echo "Error: HEAD does not match live origin/main" >&2
      exit 1
    fi
    if git rev-parse --verify --quiet "refs/tags/$release_version" >/dev/null; then
      echo "Error: local tag $release_version already exists" >&2
      exit 1
    fi
    if [[ -n $(git ls-remote --tags origin \
      "refs/tags/$release_version" "refs/tags/$release_version^{}") ]]; then
      echo "Error: remote tag $release_version already exists" >&2
      exit 1
    fi
    ;;
  tagged)
    # actions/checkout resolves a tag event to its commit but does not
    # necessarily leave the annotated tag object under refs/tags. Fetch the
    # already validated exact ref before checking its object type and target.
    if ! git fetch --force --no-tags origin \
      "refs/tags/$release_version:refs/tags/$release_version"; then
      echo "Error: could not fetch remote tag $release_version" >&2
      exit 1
    fi
    if [[ $(git cat-file -t "refs/tags/$release_version" 2>/dev/null || true) != tag ]]; then
      echo "Error: $release_version must be an annotated tag" >&2
      exit 1
    fi
    tag_commit=$(git rev-parse "refs/tags/$release_version^{commit}")
    if [[ $(git rev-parse HEAD) != "$tag_commit" ]]; then
      echo "Error: checked-out HEAD does not match $release_version" >&2
      exit 1
    fi
    if [[ $tag_commit != "$remote_head" ]]; then
      echo "Error: $release_version does not point at live origin/main" >&2
      exit 1
    fi
    remote_tag_commit=$(git ls-remote origin \
      "refs/tags/$release_version^{}" | awk '{print $1}')
    if [[ $remote_tag_commit != "$tag_commit" ]]; then
      echo "Error: remote $release_version does not resolve to the checked-out commit" >&2
      exit 1
    fi
    ;;
esac

echo "release state verified for $release_version ($mode)"

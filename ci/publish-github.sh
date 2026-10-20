#!/usr/bin/env bash
# Publish the working branch to GitHub as one clean commit and cut a release tag for it.
#
# The private history stays where it is: this script clones this repository locally, drops the
# whole tree into a single public commit, and lets ci/publicize.sh refuse to publish deployment
# leftovers. Strangers get the code, not the build machine's hostname or its registry setup.
#
# The pushed tag then triggers the `release` job in .github/workflows/agent-client.yml, which
# builds the packages and opens the GitHub Release users download from.
#
# Usage: ci/publish-github.sh [--dry-run]
#   BASE_REF         branch to publish            (default: master)
#   PUBLISH_BRANCH   remote branch                (default: main)
#   PUBLISH_REMOTE   publication target           (default: git@github.com:emporetic/agent-client.git)
#   PUBLISH_VERSION  release name                 (default: v0.0-<short sha of BASE_REF>)
set -euo pipefail

base_ref="${BASE_REF:-master}"
branch="${PUBLISH_BRANCH:-main}"
remote="${PUBLISH_REMOTE:-git@github.com:emporetic/agent-client.git}"
dry_run=0
if [ "${1:-}" = "--dry-run" ]; then dry_run=1; fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

git rev-parse --verify --quiet "$base_ref" >/dev/null || { echo "no such base ref: $base_ref" >&2; exit 1; }
dirty=$(git status --porcelain)
if [ -n "$dirty" ]; then
  printf 'a clean tree is required, publish committed work only:\n%s\n' "$dirty"
  exit 1
fi

version="v0.0-$(git rev-parse --short HEAD)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

git clone --quiet --no-hardlinks --single-branch --branch "$base_ref" "$PWD" "$work/repo"
cd "$work/repo"
git checkout --quiet --orphan publish

sh ci/publicize.sh .

git add -A
cat > "$work/message" <<EOF
client ${version}: open-source release of the agent command line client

Go source for emporetic-agent, the skill handed to coding agents under
.agents/skills/, the Pi adapter examples and the Web API contract.

Squashed from the private working branch into one commit. Self-hosted runner,
container registry and package proxy plumbing stay on the build machine.
EOF
git commit --quiet -F "$work/message"

published=$(git rev-parse --short HEAD)
echo "== published tree ($published, version $version) =="
git ls-files | sed 's/^/  /'
printf '  %s files, %s insertions\n' "$(git ls-files | wc -l | tr -d ' ')" "$(git diff --shortstat HEAD~1 2>/dev/null || git show --numstat --format= | awk '{ins+=$1} END {print ins}')"

if [ "$dry_run" = 1 ]; then
  echo "--dry-run: nothing pushed. remote=$remote branch=$branch tag=$version"
  exit 0
fi

# Repeated publications stay safe when someone else moved the public branch: read what GitHub
# serves right now, then let the lease insist on exactly that. An empty answer means the branch
# is about to appear for the first time, which is also part of the expectation.
public_head=$(GIT_TERMINAL_PROMPT=0 git ls-remote "$remote" "refs/heads/$branch" | awk '{print $1; exit}')
lease="refs/heads/${branch}:${public_head:-}"
git push --quiet "--force-with-lease=${lease}" "$remote" "HEAD:refs/heads/$branch"

# Confirm what GitHub now serves, without assuming anything about the local clone.
GIT_TERMINAL_PROMPT=0 git ls-remote "$remote" "refs/heads/$branch" | tee "$work/pushed"
[ "$(awk '{print $1}' "$work/pushed")" = "$(git rev-parse HEAD)" ] || {
  echo "the pushed head does not match this commit, inspect $branch on GitHub before continuing" >&2
  exit 1
}
git tag --annotate "$version" -m "open-source release ${version}"
git push --quiet "$remote" "refs/tags/$version"
echo "published $branch @ $published, tagged $version -> $remote"
echo "the release job runs after test passes on that tag; packages land on the GitHub Release page"

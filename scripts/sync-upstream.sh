#!/usr/bin/env bash
#
# sync-upstream.sh — rebase the gdrive-native fork onto upstream stash, then
# regenerate code, build, and test. Supervised: it stops on conflicts and never
# pushes for you.
#
#   Remotes (already configured):
#     origin  -> https://github.com/stashapp/stash.git   (upstream, read-only)
#     fork    -> https://github.com/The-OMG/stash.git     (yours)
#
# Usage:
#   scripts/sync-upstream.sh                # rebase onto the latest upstream release tag
#   scripts/sync-upstream.sh origin/develop # rebase onto a specific ref
#   scripts/sync-upstream.sh v0.28.1        # rebase onto a specific tag
#
# After it finishes cleanly:
#   git push --force-with-lease fork gdrive-native
#
set -euo pipefail

BRANCH="gdrive-native"
UPSTREAM_REMOTE="origin"

say() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\n\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

cd "$(git rev-parse --show-toplevel)"

# --- preflight ---------------------------------------------------------------
[ "$(git rev-parse --abbrev-ref HEAD)" = "$BRANCH" ] || die "checkout $BRANCH first (on $(git rev-parse --abbrev-ref HEAD))"
[ -z "$(git status --porcelain)" ] || die "working tree not clean — commit or stash first"

# Reuse recorded conflict resolutions from prior syncs (the mediapath hook edits
# recur on every release rebase — rerere replays them automatically).
git config rerere.enabled true
git config rerere.autoupdate true

say "Fetching $UPSTREAM_REMOTE (tags + branches)"
git fetch "$UPSTREAM_REMOTE" --tags --prune
# shallow clones can't compute a merge base far back — deepen once if needed
if [ -f .git/shallow ]; then
  say "Shallow clone detected — unshallowing (one-time, may take a while)"
  git fetch --unshallow "$UPSTREAM_REMOTE" || git fetch "$UPSTREAM_REMOTE" --depth=1000
fi

# --- pick the target ref -----------------------------------------------------
# Default: the latest STABLE upstream release tag (vX.Y.Z, excluding pre-releases).
# The fork tracks releases, not develop, so prod and the fork stay on the same
# schema/API line. Pass an explicit ref to override:
#   scripts/sync-upstream.sh v0.32.0        # a specific release
#   scripts/sync-upstream.sh origin/develop # bleeding edge (may drop us onto unreleased API)
latest_release() {
  git tag -l 'v*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1
}
TARGET="${1:-$(latest_release)}"
[ -n "$TARGET" ] || die "could not determine latest release tag — pass one explicitly"
say "Rebasing $BRANCH onto: $TARGET  ($(git rev-parse --short "$TARGET" 2>/dev/null || echo '?'))"
echo "  (our commits: $(git rev-list --count "$TARGET..$BRANCH" 2>/dev/null || echo '?'))"
read -r -p "Proceed with rebase? [y/N] " ans; [ "$ans" = y ] || die "aborted"

# --- rebase ------------------------------------------------------------------
git tag -f "backup/${BRANCH}-presync" "$BRANCH" >/dev/null   # safety tag to recover
if ! git rebase "$TARGET"; then
  die "rebase hit conflicts. Resolve them (git status), 'git rebase --continue', then re-run
     the codegen/build below manually. To bail out: git rebase --abort
     Recover the pre-sync state anytime: git reset --hard backup/${BRANCH}-presync

     Common conflicts on a release rebase:
       - mediapath hooks (phash.go / thumbnail.go / marker_preview.go / image/scan.go):
         take the release's version of the function, re-apply only our thin hook.
       - go.mod/go.sum: 'git checkout --ours go.mod go.sum && go mod tidy' re-derives
         our deps (google.golang.org/api, x/oauth2) at versions the release's Go allows."
fi

# --- reconcile the dependency graph ------------------------------------------
# The release may pin older Go / dep versions than the branch was built on; tidy
# re-resolves our extra deps (google.golang.org/api, x/oauth2) against it.
say "Reconciling go.mod against the release (go mod tidy)"
go mod tidy

# --- regenerate code (schema-driven) -----------------------------------------
say "Regenerating backend + frontend code (make generate + mocks)"
make generate
make generate-test-mocks

# --- build + test ------------------------------------------------------------
say "Building backend (go build ./...)"
go build ./...

say "Running fork unit tests (pkg/drive)"
go test ./pkg/drive/... || die "pkg/drive tests failed after rebase — investigate before pushing"

say "Building frontend (pnpm install + build)"
make pre-ui
( cd ui/v2.5 && NODE_OPTIONS=--max-old-space-size=6144 pnpm run check )
make ui

say "Building the stash binary (embeds the UI)"
make build

printf '\n\033[1;32m✔ Sync complete.\033[0m Rebased onto %s; code regenerated; backend+frontend build green; tests pass.\n' "$TARGET"
cat <<EOF

Next:
  1. Smoke-test the binary against a COPY of your DB.
  2. Push:   git push --force-with-lease fork $BRANCH
  3. If anything looks wrong: git reset --hard backup/${BRANCH}-presync
EOF

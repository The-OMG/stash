# Maintaining the native-Google-Drive fork

This fork (`gdrive-native`) adds native Google Drive sources to stash (see
[GOOGLE_DRIVE.md](GOOGLE_DRIVE.md)). It's designed to be a **long-lived fork** that
tracks upstream stash. This document is the playbook for keeping it current.

## Why it's cheap to maintain

The feature is **~85% additive** — brand-new files never conflict on a rebase:

- `pkg/drive/*` (engine), `pkg/mediapath`, `pkg/file/dispatch.go`
- `internal/manager/drive*.go`, `internal/api/resolver_drive.go`
- `graphql/schema/types/drive.graphql`, the `ui/.../Settings/DriveSource*` components
- `cmd/drivetest`, this doc + `GOOGLE_DRIVE.md`

Integration into existing files is deliberately **hook-based** (function variables
set at init) so the edits to core files stay small:

- `mediapath.Resolver/ProbeResolver/ThumbResolver/MetaResolver/FastResolver` — set in `internal/manager/drive.go`
- `file.DispatchFS` / `file.DriveTrasher` — the FS dispatcher + delete hook
- a few small insertions in the scanner, ffmpeg wrappers, job progress, and GraphQL/UI

The **conflict surface** is only the ~15–20 *modified* core files. Generated code
(`internal/api/generated_*.go`, `ui/.../generated-graphql.*`) is git-ignored and
rebuilt, so it never conflicts either.

## Remotes

```
origin  ->  github.com/stashapp/stash   (UPSTREAM, read-only)
fork    ->  github.com/The-OMG/stash     (yours)
```

## Routine sync (per upstream release)

```bash
git checkout gdrive-native
scripts/sync-upstream.sh              # rebases onto the latest upstream release tag
# ... resolve any conflicts if prompted, then re-run codegen/build ...
git push --force-with-lease fork gdrive-native
```

`sync-upstream.sh` fetches upstream, rebases (onto the latest `v*` tag by default —
track **releases**, not `develop`, for fewer/more-stable merges), then runs
`make generate` + mocks, `go build ./...`, `go test ./pkg/drive/...`,
`make ui`, and `make build`. It tags `backup/gdrive-native-presync` first so you can
always `git reset --hard` back. It never pushes for you.

## What to expect on a rebase

- **No conflicts** (common): additive files + untouched core → the script just
  regenerates + builds. ~1–3 hours including a smoke test. Toolchain: Go `1.25.x`,
  `pnpm@10.x`.
- **Small conflicts** (typical): upstream edited near one of our hook points. Resolve
  the few marked lines, `git rebase --continue`, finish the build.
- **Real rework** (occasional): upstream refactors a subsystem we hook into — watch
  these especially:
  - `models.FS` interface → `pkg/drive/fs.go`
  - the scanner pipeline (`internal/manager/task_scan.go`, `pkg/file/{scan,video/scan,image/scan}.go`)
  - the ffmpeg wrappers (`pkg/ffmpeg/*`) → ranged ffprobe / streaming
  - the job `Progress` API (`pkg/job/*`) → migrate/sync/scan progress
  - the file/folder stores (`pkg/sqlite/file.go`, `pkg/models/repository_file.go`) → `FindPathInfos`/`RepointFiles`
  - a **gqlgen** major bump → regenerate + fix resolver signatures
  - the frontend (Apollo/React/Vite) → the Drive Settings components

## Verifying a sync

1. `go test ./pkg/drive/...` (unit tests for the index + config parsing).
2. Point a built binary at a **copy** of a real config, and check: a source mounts,
   a selective scan creates scenes, a scene streams (HTTP 206), and — if used — a
   dry-run migration reports sane counts.
3. Only then `git push --force-with-lease fork gdrive-native`.

## CI

`.github/workflows/fork-ci.yml` builds + tests the branch on every push, and runs a
**weekly upstream-compatibility check** (dry-merge of upstream `develop`, build) so
you find out early — on your schedule — when upstream changes break us.

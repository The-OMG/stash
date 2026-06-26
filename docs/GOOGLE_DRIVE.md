# Native Google Drive sources

This fork adds first-class Google Drive shared-drive ("team drive") sources to
stash. Instead of pointing stash at an `rclone` FUSE mount, stash talks to the
Google Drive API directly:

- **Indexing** is done with the Drive `files.list` API (≈1 call per 1000 files),
  and kept up to date with the **Changes API** — so re-scanning a 400k-file
  library to find new/changed/deleted files takes a few API calls (sub-second)
  instead of re-walking the whole tree over FUSE.
- **Hashing** uses stash's `oshash` (first+last 64 KiB + size), served by ranged
  `GET`s — no whole-file downloads to index.
- **Media** (playback, transcoding, sprites, previews, phash, thumbnails) is
  served from an on-demand local **LRU cache**: the file is downloaded the first
  time ffmpeg/the player needs it, then evicted by least-recently-used once the
  cache exceeds its size cap. No FUSE mount anywhere.

## Authentication

Auth uses **Google service-account JSON** credentials. Point a source at either:

- a single service-account `.json` file, or
- a **directory** of service-account `.json` files — these are used as a pool and
  rotated to spread Drive API quota (handy for very large libraries / heavy media
  caching).

The service account(s) must be granted access to the shared drive (add the
`client_email` as a member of the shared drive, or use a domain-wide-delegated
account).

## Configuring sources

### From the UI

**Settings → Library → Google Drive Sources.** Add a source with:

| Field | Meaning |
|-------|---------|
| ID | stable identifier; used in the virtual library path `/__gdrive__/<id>` |
| Name | display name |
| Shared drive id | the team-drive id (e.g. `0AEFojjZ0gu-9Uk9PVA`) |
| Service-account JSON | path to a `.json` file or a directory of them |
| OAuth scope | optional; defaults to full Drive. Use `https://www.googleapis.com/auth/drive.readonly` for read-only |
| Cache directory | optional; defaults to `<cache>/gdrive/<id>` |

Adding a source validates Drive access immediately, mounts it, and it is included
in the next **Scan**.

### Sidecar config (equivalent)

Sources are stored in `<config>/gdrive_sources.json`:

```json
{
  "sources": [
    {
      "id": "tv",
      "name": "TV Teamdrive",
      "drive_id": "0AEFojjZ0gu-9Uk9PVA",
      "keys_path": "/home/theomg/keys",
      "scope": "https://www.googleapis.com/auth/drive.readonly",
      "cache_dir": "",
      "cache_bytes": 53687091200
    }
  ]
}
```

`cache_bytes` defaults to 50 GiB if unset/zero.

## How a scan works

1. **First scan (cold index):** the entire shared drive is enumerated into a
   local SQLite index (`<config>/gdrive/<id>.sqlite`) and a Changes page-token is
   stored. (~9.5 min for ~250k items in testing.)
2. **Subsequent scans:** only the Drive **Changes** since the stored token are
   applied — additions, edits, and trashes/removals — then stash's normal scan
   and cleanup run over the (now-current) index. Deletions on the drive are
   detected and cleaned up without a full re-walk.

## Architecture

| Concern | Location |
|---------|----------|
| Auth (SA pool, rotation) | `pkg/drive/auth.go` |
| Drive client (list, changes, ranged read, backoff) | `pkg/drive/client.go` |
| SQLite id↔path index + page token | `pkg/drive/index.go` |
| Full/incremental sync | `pkg/drive/source.go` |
| `models.FS` over Drive (ranged reads) | `pkg/drive/fs.go` |
| LRU media cache | `pkg/drive/cache.go` |
| Path-prefix FS dispatcher (Drive vs local) | `pkg/file/dispatch.go` |
| Media path resolver hook | `pkg/mediapath/mediapath.go` |
| Source bootstrap / mount / sync / CRUD | `internal/manager/drive.go` |
| GraphQL resolvers | `internal/api/resolver_drive.go` |
| Settings UI | `ui/v2.5/src/components/Settings/DriveSourcesSection.tsx` |

Drive-backed library paths use the virtual prefix `/__gdrive__/<id>/…`; the
dispatching file system routes those to the Drive backend and everything else to
the local disk, so local and Drive libraries coexist transparently.

## Limitations / notes

- Generating sprites/previews/phash for Drive videos downloads the whole file to
  the cache (ffmpeg needs a seekable file) — expected, and bounded by the cache
  cap.
- Zip/gallery archives inside Drive are not opened over the API (they would need
  the whole file as a `ReaderAt`); such files are skipped by the scanner.
- `md5` hashing is best left disabled for Drive sources (whole-file read); the
  Drive `md5Checksum` is captured in the index for free if needed later.

## Validation harness

`cmd/drivetest` exercises the engine against a real shared drive:

```
go run ./cmd/drivetest -keys /path/to/sa.json -drive <DRIVE_ID>            # full list + change token
go run ./cmd/drivetest -keys ... -drive <DRIVE_ID> -since <TOKEN>          # incremental change poll
go run ./cmd/drivetest -keys ... -drive <DRIVE_ID> -fstest                 # DriveFS ranged-read oshash check
```

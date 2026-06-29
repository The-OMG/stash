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

Two options — use whichever fits:

### Google OAuth (recommended for most users)

Connect your own Google account and pick from your accessible shared drives.

1. In the [Google Cloud Console](https://console.cloud.google.com/apis/credentials)
   enable the **Drive API** and create an **OAuth client → Web application**.
2. Add the redirect URI shown in **Settings → Library → Google Drive → Connect**
   (it's `<your-stash-url>/oauth/google/callback`) to the client.
3. Paste the client ID/secret into stash (or ship a built-in client by setting
   `builtinGoogleClientID/Secret` in `internal/manager/drive_oauth.go`), then click
   **Connect Google Drive** and approve. The refresh token is stored in
   `<config>/gdrive_oauth.json` (mode 0600).

Sources added "via the connected account" use `auth_type: oauth` and reference the
connected token — no per-source credentials. You can **Edit OAuth client**,
**Disconnect**, or **Clear client & reset** from the same screen.

### Service account(s)

Point a source at either:

- a single service-account `.json` file, or
- a **directory** of service-account `.json` files — used as a pool and rotated to
  spread Drive API quota (handy for very large libraries / heavy media caching).

The service account(s) must be granted access to the shared drive (add the
`client_email` as a member, or use a domain-wide-delegated account). Service
accounts are offered as the **Advanced** option in the Add-source dialog.

## Configuring sources

### From the UI

**Settings → Library → Google Drive** has three steps:

1. **Connect** — set up the OAuth client (above) and connect your account
   (optional if you only use service accounts).
2. **Sources** — **Add Drive source…** opens a picker. With a connected account,
   choose from your shared drives and browse into a sub-folder to scope it; or
   switch to **Service account (advanced)** and enter a keys path + drive id.
   Per-source options include **Fast scan** (use Drive's duration/dimensions and
   skip ffprobe — faster, no codec detail).
3. **Migrate** — repoint an existing rclone-mounted library onto a source by path
   (see below).

Each source gets a stable **ID** used in the virtual library path
`/__gdrive__/<id>`. Adding a source validates Drive access immediately and mounts
it. Use **Index** on a source to pre-build/refresh its index without a full scan;
**Remove** unmounts it.

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
      "cache_bytes": 53687091200,
      "auth_type": "sa",
      "fast_scan": false
    }
  ]
}
```

`cache_bytes` defaults to 50 GiB if unset/zero (per source, LRU-evicted).
`auth_type` is `sa` (default) or `oauth` (uses the connected account, no
`keys_path` needed). `fast_scan` skips ffprobe using Drive's native metadata.

## How a scan works

1. **First scan (cold index):** the entire shared drive is enumerated into a
   local SQLite index (`<config>/gdrive/<id>.sqlite`) and a Changes page-token is
   stored. (~9.5 min for ~250k items in testing.)
2. **Subsequent scans:** only the Drive **Changes** since the stored token are
   applied — additions, edits, and trashes/removals — then stash's normal scan
   and cleanup run over the (now-current) index. Deletions on the drive are
   detected and cleaned up without a full re-walk.

## Migrating an existing (rclone-mounted) library

If your library is already scanned from an rclone mount, you can repoint those
scenes onto a native Drive source **without re-scanning or re-hashing** — file
ids, fingerprints, and all scene/performer/tag links are preserved; only each
file's path changes.

**Settings → Library → Google Drive → Migrate:** enter the existing path prefix
(e.g. `/home/theomg/cloud`) and the target Drive source, then:

- **Preview (dry run)** — reports `candidates`, `would migrate`, `not in DB`,
  `size mismatch`, `collision` without changing anything.
- **Migrate** — repoints matched files.

Both run as background **jobs** (visible on the Tasks page with live progress);
the last result is shown in the Migrate panel via `driveMigrateStatus`. Matching
is by **relative path + size** (`require_size`, on by default) against the drive
index, so only files that genuinely exist on the target drive are touched. Run
the migration **before** scanning a drive to avoid creating duplicate entries.

Mechanics: the drive index is iterated and joined against a one-shot lightweight
`path → size` map of the DB (`FileStore.FindPathSizes`); matched files have their
`parent_folder_id` repointed to the drive folder (created as needed).

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

- **Scrub sprites** are generated **on demand**: the first time a Drive-backed
  scene's scrubber/VTT is requested, the sprite + thumbs VTT are generated and
  cached (deduped per scene). Generating sprites/previews/phash downloads the
  whole file to the cache (ffmpeg needs a seekable file) — expected, and bounded
  by the cache cap. (A future optimization is to seek over the ranged URL and read
  only the needed segments rather than the whole file.)
- **Cover thumbnails** are served directly from Drive's `thumbnailLink` (no
  download). Durations/resolutions come free from Drive's `videoMediaMetadata`.
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

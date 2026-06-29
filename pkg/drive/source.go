package drive

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gdrive "google.golang.org/api/drive/v3"
)

// thumbClient fetches Drive thumbnail images with a bounded timeout.
var thumbClient = &http.Client{Timeout: 30 * time.Second}

// thumbSizeRe matches a trailing Drive thumbnail size token, e.g. "=s220" or
// "=s220-c".
var thumbSizeRe = regexp.MustCompile(`=s\d+(-[a-z0-9]+)*$`)

// downloadURLFmt is the Drive API media-download endpoint. ffprobe/ffmpeg can
// read this URL with an Authorization header and issue HTTP range requests, so
// metadata probing only fetches the moov atom rather than the whole file.
const downloadURLFmt = "https://www.googleapis.com/drive/v3/files/%s?alt=media&supportsAllDrives=true"

// ProbeTarget returns an authenticated download URL and the HTTP headers ffprobe
// needs to read fileID directly (ranged), without a full local download.
func (s *Source) ProbeTarget(ctx context.Context, fileID string) (string, []string, error) {
	tok, err := s.Pool.Token(ctx)
	if err != nil {
		return "", nil, err
	}
	url := fmt.Sprintf(downloadURLFmt, fileID)
	return url, []string{"Authorization: Bearer " + tok}, nil
}

// ThumbnailData downloads Drive's own thumbnail for a file (a small JPEG),
// optionally resized, so covers and image thumbnails don't require downloading
// the full file or running ffmpeg/vips.
func (s *Source) ThumbnailData(ctx context.Context, fileID string, size int) ([]byte, error) {
	svc, err := s.Pool.Next(ctx)
	if err != nil {
		return nil, err
	}
	link, err := ThumbnailURL(ctx, svc, fileID)
	if err != nil {
		return nil, err
	}
	if link == "" {
		return nil, fmt.Errorf("no thumbnail available for %s", fileID)
	}
	if size > 0 {
		link = resizeThumbLink(link, size)
	}

	tok, err := s.Pool.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := thumbClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("thumbnail fetch for %s: %s", fileID, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// guard against an empty/HTML error body being stored as a cover
	if len(data) == 0 || !strings.HasPrefix(http.DetectContentType(data), "image/") {
		return nil, fmt.Errorf("drive thumbnail for %s is not an image", fileID)
	}
	return data, nil
}

// resizeThumbLink adjusts the "=sNNN" size suffix on a Drive thumbnail link,
// leaving the link unchanged when no such suffix is present.
func resizeThumbLink(link string, size int) string {
	if thumbSizeRe.MatchString(link) {
		return thumbSizeRe.ReplaceAllString(link, "=s"+strconv.Itoa(size))
	}
	return link
}

// StreamRange issues a ranged GET to Drive and returns the raw HTTP response
// (200 or 206), so a handler can proxy the byte range straight to the browser
// without downloading the whole file. Caller must close resp.Body.
func (s *Source) StreamRange(ctx context.Context, fileID, rangeHeader string) (*http.Response, error) {
	svc, err := s.Pool.Next(ctx)
	if err != nil {
		return nil, err
	}
	var resp *http.Response
	err = retryable(func() error {
		call := svc.Files.Get(fileID).SupportsAllDrives(true)
		if rangeHeader != "" {
			call.Header().Set("Range", rangeHeader)
		}
		var e error
		resp, e = call.Context(ctx).Download()
		return e
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// SetTrashed moves a Drive file to (or restores it from) the drive's trash.
// Drive trash is reversible, which lets it back stash's delete rollback.
func (s *Source) SetTrashed(ctx context.Context, fileID string, trashed bool) error {
	svc, err := s.Pool.Next(ctx)
	if err != nil {
		return err
	}
	return retryable(func() error {
		_, e := svc.Files.Update(fileID, &gdrive.File{
			Trashed:         trashed,
			ForceSendFields: []string{"Trashed"},
		}).SupportsAllDrives(true).Context(ctx).Do()
		return e
	})
}

// ReadHead returns up to the first n bytes of fileID via a ranged GET. Used for
// container magic-byte detection without a full download.
func (s *Source) ReadHead(ctx context.Context, fileID string, n int) ([]byte, error) {
	svc, err := s.Pool.Next(ctx)
	if err != nil {
		return nil, err
	}
	rc, err := OpenRange(ctx, svc, fileID, 0)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	buf := make([]byte, n)
	read, err := io.ReadFull(rc, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:read], nil
}

// Source ties a shared drive to its persistent index and credential pool. It
// is the unit stash registers as a library.
type Source struct {
	DriveID string
	Pool    Authenticator
	Index   *Index
}

// SyncStats reports what a sync did.
type SyncStats struct {
	Full    bool // true if this was a cold full index
	Added   int  // items inserted/updated during a full sync
	Updated int  // items changed during an incremental sync
	Removed int  // items deleted/trashed during an incremental sync
	Total   int  // total files in the index afterwards
}

// Sync performs a cold full index if no change token exists yet, otherwise a
// fast incremental sync driven by the Drive Changes API.
func (s *Source) Sync(ctx context.Context) (SyncStats, error) {
	token, err := s.Index.PageToken()
	if err != nil {
		return SyncStats{}, err
	}
	if token == "" {
		return s.initialSync(ctx)
	}
	return s.incrementalSync(ctx, token)
}

// SyncFull is the explicit "Sync" action: a full enumeration the first time
// (so the index/file_count is populated), then incremental change syncs.
func (s *Source) SyncFull(ctx context.Context, onProgress func(indexed int)) (SyncStats, error) {
	token, err := s.Index.PageToken()
	if err != nil {
		return SyncStats{}, err
	}
	// Full-index unless a previous full enumeration COMPLETED (tracked by a flag,
	// so a partial/interrupted index or a lazy token-only state is retried).
	done, _ := s.Index.FullIndexDone()
	if !done || token == "" {
		return s.FullIndex(ctx, onProgress)
	}
	return s.incrementalSync(ctx, token)
}

// FullIndex enumerates the whole source into the index (so file_count reflects
// the drive) and marks folders listed. Whole-drive sources use the flat
// files.list; folder-scoped sources walk from the root. The change token is
// captured first so edits during the (long) enumeration are caught next sync.
func (s *Source) FullIndex(ctx context.Context, onProgress func(indexed int)) (SyncStats, error) {
	svc, err := s.Pool.First(ctx)
	if err != nil {
		return SyncStats{}, err
	}
	startToken, err := StartPageToken(ctx, svc, s.DriveID)
	if err != nil {
		return SyncStats{}, err
	}

	report := func(n int) {
		if onProgress != nil {
			onProgress(n)
		}
	}

	stats := SyncStats{Full: true}
	if s.Index.IsWholeDrive() {
		// flat enumeration of the whole drive (fewest API calls)
		err = ListDrive(ctx, svc, s.DriveID, func(batch []Item) error {
			if e := s.Index.Upsert(batch); e != nil {
				return e
			}
			stats.Added += len(batch)
			report(stats.Added)
			return ctx.Err()
		})
		if err != nil {
			return stats, err
		}
		if err := s.Index.MarkAllFoldersListed(); err != nil {
			return stats, err
		}
	} else {
		// scoped: breadth-first walk from the root folder
		queue := []string{s.Index.RootID()}
		for len(queue) > 0 {
			folder := queue[0]
			queue = queue[1:]
			if err := ListFolder(ctx, svc, s.DriveID, folder, func(batch []Item) error {
				if e := s.Index.Upsert(batch); e != nil {
					return e
				}
				stats.Added += len(batch)
				report(stats.Added)
				for _, it := range batch {
					if it.IsFolder {
						queue = append(queue, it.ID)
					}
				}
				return ctx.Err()
			}); err != nil {
				return stats, err
			}
			if err := s.Index.MarkListed(folder); err != nil {
				return stats, err
			}
		}
	}

	if err := s.Index.SetPageToken(startToken); err != nil {
		return stats, err
	}
	if err := s.Index.SetFullIndexDone(); err != nil {
		return stats, err
	}
	stats.Total, _ = s.Index.Count()
	return stats, nil
}

// initialSync prepares a brand-new source by capturing the change page token.
// It deliberately does NOT pre-enumerate the whole drive: the index is
// populated lazily, folder-by-folder, by DriveFS.ReadDir as the scanner walks,
// so scenes are created continuously during the first scan instead of after a
// long full-enumeration. Capturing the token up front means any edits during
// the first walk are picked up by the next incremental sync.
func (s *Source) initialSync(ctx context.Context) (SyncStats, error) {
	svc, err := s.Pool.First(ctx)
	if err != nil {
		return SyncStats{}, err
	}

	startToken, err := StartPageToken(ctx, svc, s.DriveID)
	if err != nil {
		return SyncStats{}, err
	}
	if err := s.Index.SetPageToken(startToken); err != nil {
		return SyncStats{}, err
	}

	n, _ := s.Index.Count()
	return SyncStats{Full: true, Total: n}, nil
}

// incrementalSync applies only the deltas since the stored token: a handful of
// API calls regardless of library size. This is what turns a 400k-file
// change-scan from hours into seconds.
func (s *Source) incrementalSync(ctx context.Context, token string) (SyncStats, error) {
	svc, err := s.Pool.First(ctx)
	if err != nil {
		return SyncStats{}, err
	}

	changes, newToken, err := ListChanges(ctx, svc, s.DriveID, token)
	if err != nil {
		return SyncStats{}, err
	}

	var stats SyncStats
	var upserts []Item
	for _, c := range changes {
		if c.Removed || (c.Item != nil && c.Item.Trashed) {
			if err := s.Index.Delete(c.FileID); err != nil {
				return stats, err
			}
			stats.Removed++
			continue
		}
		if c.Item != nil {
			upserts = append(upserts, *c.Item)
			stats.Updated++
		}
	}
	if len(upserts) > 0 {
		if err := s.Index.Upsert(upserts); err != nil {
			return stats, err
		}
	}

	if err := s.Index.SetPageToken(newToken); err != nil {
		return stats, err
	}
	stats.Total, _ = s.Index.Count()
	return stats, nil
}

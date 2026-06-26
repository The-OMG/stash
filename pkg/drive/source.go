package drive

import (
	"context"
	"fmt"
)

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

// Source ties a shared drive to its persistent index and credential pool. It
// is the unit stash registers as a library.
type Source struct {
	DriveID string
	Pool    *SAPool
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
		return s.fullSync(ctx)
	}
	return s.incrementalSync(ctx, token)
}

// fullSync enumerates the entire drive. The change token is captured BEFORE
// listing so any edits made during the (potentially long) listing are picked
// up by the next incremental sync rather than lost.
func (s *Source) fullSync(ctx context.Context) (SyncStats, error) {
	svc, err := s.Pool.First(ctx)
	if err != nil {
		return SyncStats{}, err
	}

	startToken, err := StartPageToken(ctx, svc, s.DriveID)
	if err != nil {
		return SyncStats{}, err
	}

	stats := SyncStats{Full: true}
	err = ListDrive(ctx, svc, s.DriveID, func(batch []Item) error {
		if err := s.Index.Upsert(batch); err != nil {
			return err
		}
		stats.Added += len(batch)
		return nil
	})
	if err != nil {
		return stats, err
	}

	if err := s.Index.SetPageToken(startToken); err != nil {
		return stats, err
	}
	stats.Total, _ = s.Index.Count()
	return stats, nil
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

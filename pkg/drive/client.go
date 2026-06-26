package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// folderMime is the Drive MIME type for folders.
const folderMime = "application/vnd.google-apps.folder"

// fileFields is the minimal set of fields requested per file. Keeping this
// small keeps listings fast and cheap. md5Checksum comes free from metadata,
// avoiding any file read for hashing fallback.
const fileFields = "id,name,size,mimeType,modifiedTime,md5Checksum,parents,trashed"

// Item is a flattened Drive file/folder record as stored in the index.
type Item struct {
	ID       string
	Name     string
	Parent   string // first parent id ("" for the drive root)
	Size     int64
	MimeType string
	ModTime  time.Time
	MD5      string
	IsFolder bool
	Trashed  bool
}

func itemFromFile(f *drive.File) Item {
	var parent string
	if len(f.Parents) > 0 {
		parent = f.Parents[0]
	}
	mt, _ := time.Parse(time.RFC3339, f.ModifiedTime)
	return Item{
		ID:       f.Id,
		Name:     f.Name,
		Parent:   parent,
		Size:     f.Size,
		MimeType: f.MimeType,
		ModTime:  mt,
		MD5:      f.Md5Checksum,
		IsFolder: f.MimeType == folderMime,
		Trashed:  f.Trashed,
	}
}

// retryable wraps a Drive API call with exponential backoff on rate-limit
// (403 userRateLimitExceeded / 429) and transient 5xx errors.
func retryable(fn func() error) error {
	const maxAttempts = 6
	delay := 500 * time.Millisecond

	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}

		var gerr *googleapi.Error
		if errors.As(err, &gerr) && (gerr.Code == 403 || gerr.Code == 429 || gerr.Code >= 500) {
			time.Sleep(delay)
			delay *= 2
			continue
		}
		return err
	}
	return err
}

// ListDrive enumerates every non-trashed item in a shared drive, invoking cb
// once per page (up to 1000 items). A 400k-file drive is ~400 calls — well
// within a single SA's quota — so a stable service is used for consistent
// pagination.
func ListDrive(ctx context.Context, svc *drive.Service, driveID string, cb func([]Item) error) error {
	pageToken := ""
	for {
		var resp *drive.FileList
		err := retryable(func() error {
			call := svc.Files.List().
				DriveId(driveID).
				Corpora("drive").
				IncludeItemsFromAllDrives(true).
				SupportsAllDrives(true).
				Q("trashed=false").
				PageSize(1000).
				Fields(googleapi.Field("nextPageToken,files(" + fileFields + ")"))
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			var e error
			resp, e = call.Context(ctx).Do()
			return e
		})
		if err != nil {
			return err
		}

		batch := make([]Item, 0, len(resp.Files))
		for _, f := range resp.Files {
			batch = append(batch, itemFromFile(f))
		}
		if err := cb(batch); err != nil {
			return err
		}

		if resp.NextPageToken == "" {
			return nil
		}
		pageToken = resp.NextPageToken
	}
}

// OpenRange opens a read stream for a file's content starting at offset
// (0 = whole file). This backs both oshash's cheap head+tail reads and the
// media cache's full downloads.
func OpenRange(ctx context.Context, svc *drive.Service, fileID string, offset int64) (io.ReadCloser, error) {
	var resp *http.Response
	err := retryable(func() error {
		call := svc.Files.Get(fileID).SupportsAllDrives(true)
		if offset > 0 {
			call.Header().Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		var e error
		resp, e = call.Context(ctx).Download()
		return e
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ListFolder enumerates the direct children of folderID, invoking cb per page.
// Used for lazy, walk-driven indexing so scenes are created as the scan
// descends rather than after a full pre-enumeration.
func ListFolder(ctx context.Context, svc *drive.Service, driveID, folderID string, cb func([]Item) error) error {
	q := fmt.Sprintf("'%s' in parents and trashed=false", folderID)
	pageToken := ""
	for {
		var resp *drive.FileList
		err := retryable(func() error {
			call := svc.Files.List().
				DriveId(driveID).
				Corpora("drive").
				IncludeItemsFromAllDrives(true).
				SupportsAllDrives(true).
				Q(q).
				PageSize(1000).
				Fields(googleapi.Field("nextPageToken,files(" + fileFields + ")"))
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			var e error
			resp, e = call.Context(ctx).Do()
			return e
		})
		if err != nil {
			return err
		}

		batch := make([]Item, 0, len(resp.Files))
		for _, f := range resp.Files {
			batch = append(batch, itemFromFile(f))
		}
		if err := cb(batch); err != nil {
			return err
		}

		if resp.NextPageToken == "" {
			return nil
		}
		pageToken = resp.NextPageToken
	}
}

// StartPageToken returns the current change token for a shared drive. Persist
// this after a full index; subsequent scans poll changes from it.
func StartPageToken(ctx context.Context, svc *drive.Service, driveID string) (string, error) {
	var resp *drive.StartPageToken
	err := retryable(func() error {
		var e error
		resp, e = svc.Changes.GetStartPageToken().
			DriveId(driveID).
			SupportsAllDrives(true).
			Context(ctx).Do()
		return e
	})
	if err != nil {
		return "", err
	}
	return resp.StartPageToken, nil
}

// Change is a single delta from the Changes API. Removed (or Item.Trashed)
// means the file is gone from the drive's view and should be cleaned up.
type Change struct {
	FileID  string
	Removed bool
	Item    *Item // nil when Removed and no file metadata is available
}

// ListChanges fetches all changes since pageToken and returns the deltas plus
// the next token to persist. This is the core of fast incremental scanning:
// instead of re-walking the whole tree, only changed/removed files are
// returned.
func ListChanges(ctx context.Context, svc *drive.Service, driveID, pageToken string) ([]Change, string, error) {
	var changes []Change
	token := pageToken
	debug := os.Getenv("DRIVE_DEBUG") != ""
	page := 0

	for {
		page++
		if debug {
			fmt.Fprintf(os.Stderr, "[changes] page %d, token=%s, accumulated=%d\n", page, token, len(changes))
		}
		var resp *drive.ChangeList
		err := retryable(func() error {
			var e error
			resp, e = svc.Changes.List(token).
				DriveId(driveID).
				IncludeItemsFromAllDrives(true).
				SupportsAllDrives(true).
				IncludeRemoved(true).
				PageSize(1000).
				Fields(googleapi.Field("nextPageToken,newStartPageToken,changes(fileId,removed,file(" + fileFields + "))")).
				Context(ctx).Do()
			return e
		})
		if err != nil {
			return nil, "", err
		}

		for _, c := range resp.Changes {
			ch := Change{FileID: c.FileId, Removed: c.Removed}
			if c.File != nil {
				it := itemFromFile(c.File)
				ch.Item = &it
			}
			changes = append(changes, ch)
		}

		// newStartPageToken on the final page is the token to persist for the
		// next poll.
		if resp.NewStartPageToken != "" {
			return changes, resp.NewStartPageToken, nil
		}
		if resp.NextPageToken == "" {
			return changes, token, nil
		}
		token = resp.NextPageToken
	}
}

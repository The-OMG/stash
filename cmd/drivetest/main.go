// Command drivetest exercises the pkg/drive engine against a real shared
// drive: it times a full listing, counts files/folders/bytes, and fetches a
// change start-page token. This validates service-account auth and the Drive
// API path independently of the rest of stash.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/stashapp/stash/pkg/drive"
	"github.com/stashapp/stash/pkg/hash/oshash"
	gdrive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

func main() {
	keys := flag.String("keys", "/home/theomg/keys", "service-account JSON file or directory")
	driveID := flag.String("drive", "", "shared (team) drive id")
	scope := flag.String("scope", "", "oauth scope (default: full drive)")
	since := flag.String("since", "", "change page token: poll incremental changes instead of full list")
	fstest := flag.Bool("fstest", false, "validate DriveFS ranged reads: oshash one file via head+tail vs whole-file download")
	flag.Parse()

	if *driveID == "" {
		fmt.Fprintln(os.Stderr, "usage: drivetest -drive <DRIVE_ID> [-keys <path>]")
		os.Exit(2)
	}

	ctx := context.Background()

	pool, err := drive.NewSAPool(*keys, *scope)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("loaded %d service account(s) from %s\n", pool.Len(), *keys)

	svc, err := pool.First(ctx)
	if err != nil {
		fatal(err)
	}

	// Incremental change-poll mode: this is what every scan after the first
	// one does instead of re-walking the whole tree.
	if *since != "" {
		t := time.Now()
		changes, newToken, err := drive.ListChanges(ctx, svc, *driveID, *since)
		if err != nil {
			fatal(err)
		}
		var added, removed int
		for _, c := range changes {
			if c.Removed || (c.Item != nil && c.Item.Trashed) {
				removed++
			} else {
				added++
			}
		}
		fmt.Printf("incremental poll: %d changes (%d upserts, %d deletions) in %s\n",
			len(changes), added, removed, time.Since(t).Round(time.Millisecond))
		fmt.Printf("next token: %s\n", newToken)
		return
	}

	if *fstest {
		runFSTest(ctx, pool, svc, *driveID)
		return
	}

	var files, folders, pages int
	var bytes int64
	start := time.Now()

	err = drive.ListDrive(ctx, svc, *driveID, func(batch []drive.Item) error {
		pages++
		for _, it := range batch {
			if it.IsFolder {
				folders++
			} else {
				files++
				bytes += it.Size
			}
		}
		if pages%25 == 0 {
			fmt.Printf("  ...%d items so far (%s)\n", files+folders, time.Since(start).Round(time.Second))
		}
		return nil
	})
	if err != nil {
		fatal(err)
	}

	elapsed := time.Since(start)
	fmt.Printf("\nlisted %d files + %d folders (%0.1f GiB) in %d API pages, %s\n",
		files, folders, float64(bytes)/(1<<30), pages, elapsed.Round(time.Millisecond))
	if elapsed > 0 {
		fmt.Printf("rate: %.0f items/sec\n", float64(files+folders)/elapsed.Seconds())
	}

	token, err := drive.StartPageToken(ctx, svc, *driveID)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("change start-page token: %s\n", token)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// runFSTest proves the DriveFS read path: it picks the smallest real file in
// the drive, computes its oshash through DriveFS (which uses ranged head+tail
// GETs) and again from a full download, and checks they match.
func runFSTest(ctx context.Context, pool *drive.SAPool, svc *gdrive.Service, driveID string) {
	// find a small, non-empty file to keep the reference download cheap.
	resp, err := svc.Files.List().
		DriveId(driveID).Corpora("drive").
		IncludeItemsFromAllDrives(true).SupportsAllDrives(true).
		Q("trashed=false and (mimeType='image/jpeg' or mimeType='image/png')").
		PageSize(1000).
		Fields(googleapi.Field("files(id,name,size,parents,md5Checksum)")).
		Context(ctx).Do()
	if err != nil {
		fatal(err)
	}
	// Pick a file large enough that oshash's head and tail are distinct 64KB
	// ranges (>128KB), but small enough to download cheaply for the reference.
	const minSize, maxSize = 256 << 10, 64 << 20
	var chosen *gdrive.File
	for _, f := range resp.Files {
		if f.Size >= minSize && f.Size <= maxSize {
			chosen = f
			break
		}
	}
	if chosen == nil { // fall back to any non-empty file
		for _, f := range resp.Files {
			if f.Size > 0 {
				chosen = f
				break
			}
		}
	}
	if chosen == nil {
		fatal(fmt.Errorf("no non-empty file found to test"))
	}
	fmt.Printf("test file: %q (%d bytes, id=%s)\n", chosen.Name, chosen.Size, chosen.Id)

	// build a throwaway index containing just this file at the drive root.
	tmp, err := os.MkdirTemp("", "drivefs-test")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(tmp)

	index, err := drive.OpenIndex(filepath.Join(tmp, "idx.sqlite"), driveID)
	if err != nil {
		fatal(err)
	}
	defer index.Close()

	if err := index.Upsert([]drive.Item{{
		ID: chosen.Id, Name: chosen.Name, Parent: driveID, Size: chosen.Size, MD5: chosen.Md5Checksum,
	}}); err != nil {
		fatal(err)
	}

	source := &drive.Source{DriveID: driveID, Pool: pool, Index: index}
	dfs := drive.NewDriveFS(source, "/t")

	// (1) oshash through DriveFS (ranged head+tail reads)
	t := time.Now()
	fh, err := dfs.Open("/t/" + chosen.Name)
	if err != nil {
		fatal(err)
	}
	rs, ok := fh.(io.ReadSeeker)
	if !ok {
		fatal(fmt.Errorf("driveFile is not an io.ReadSeeker"))
	}
	viaFS, err := oshash.FromReader(rs, chosen.Size)
	fh.Close()
	if err != nil {
		fatal(err)
	}
	fsDur := time.Since(t)

	// (2) reference oshash from a full download
	rc, err := drive.OpenRange(ctx, svc, chosen.Id, 0)
	if err != nil {
		fatal(err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		fatal(err)
	}
	ref, err := oshash.FromReader(byteSeeker(data), int64(len(data)))
	if err != nil {
		fatal(err)
	}

	fmt.Printf("oshash via DriveFS (ranged):   %s  (%s)\n", viaFS, fsDur.Round(time.Millisecond))
	fmt.Printf("oshash via full download:      %s\n", ref)
	if viaFS == ref {
		fmt.Println("MATCH ✓  DriveFS ranged reads are correct")
	} else {
		fmt.Println("MISMATCH ✗")
		os.Exit(1)
	}
}

// byteSeeker wraps a byte slice as an io.ReadSeeker.
func byteSeeker(b []byte) io.ReadSeeker { return &sliceRS{b: b} }

type sliceRS struct {
	b   []byte
	pos int64
}

func (s *sliceRS) Read(p []byte) (int, error) {
	if s.pos >= int64(len(s.b)) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.pos:])
	s.pos += int64(n)
	return n, nil
}

func (s *sliceRS) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = off
	case io.SeekCurrent:
		s.pos += off
	case io.SeekEnd:
		s.pos = int64(len(s.b)) + off
	}
	return s.pos, nil
}

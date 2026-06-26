package drive

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/stashapp/stash/pkg/models"
	gdrive "google.golang.org/api/drive/v3"
)

// errNotSupported is returned for operations Drive cannot serve through the FS
// abstraction (e.g. opening a zip inside Drive).
var errNotSupported = errors.New("operation not supported on drive fs")

// DriveFS implements models.FS backed by a Source's persistent index and the
// Drive API. It is mounted at a virtual root path (the stash library path);
// incoming names are translated to drive-relative paths and resolved against
// the index. Directory listings come from the local SQLite index (no network);
// file content is streamed via ranged GETs.
type DriveFS struct {
	source *Source
	root   string // virtual library root this FS is mounted at, e.g. /$gdrive/<id>
}

// NewDriveFS mounts a Source at the given virtual root path.
func NewDriveFS(source *Source, root string) *DriveFS {
	return &DriveFS{source: source, root: filepath.Clean(root)}
}

// Root returns the virtual library path this FS is mounted at.
func (d *DriveFS) Root() string { return d.root }

// rel converts an absolute stash path to a drive-relative slash path.
func (d *DriveFS) rel(name string) (string, error) {
	name = filepath.Clean(name)
	if name == d.root {
		return "", nil
	}
	r, err := filepath.Rel(d.root, name)
	if err != nil || r == ".." || filepath.IsAbs(r) {
		return "", fs.ErrNotExist
	}
	return filepath.ToSlash(r), nil
}

func (d *DriveFS) lookup(name string) (Item, error) {
	rel, err := d.rel(name)
	if err != nil {
		return Item{}, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	it, ok, err := d.source.Index.LookupPath(rel)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		return Item{}, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	// the synthetic root has no item name; synthesise a folder item.
	if rel == "" {
		it.IsFolder = true
		it.Name = filepath.Base(d.root)
	}
	return it, nil
}

func (d *DriveFS) Stat(name string) (fs.FileInfo, error) {
	it, err := d.lookup(name)
	if err != nil {
		return nil, err
	}
	return &driveFileInfo{it: it}, nil
}

// Lstat is identical to Stat: Drive has no symlinks.
func (d *DriveFS) Lstat(name string) (fs.FileInfo, error) {
	return d.Stat(name)
}

func (d *DriveFS) Open(name string) (fs.ReadDirFile, error) {
	it, err := d.lookup(name)
	if err != nil {
		return nil, err
	}
	return &driveFile{fsys: d, it: it}, nil
}

// OpenZip is unsupported: zip-in-drive would require the whole file as a
// ReaderAt. Returning an error makes the scanner skip zip contents gracefully.
func (d *DriveFS) OpenZip(name string, size int64) (models.ZipFS, error) {
	return nil, errNotSupported
}

// IsPathCaseSensitive reports true; Drive treats names case-sensitively.
func (d *DriveFS) IsPathCaseSensitive(path string) (bool, error) {
	return true, nil
}

// service returns a Drive service for content reads.
func (d *DriveFS) service(ctx context.Context) (*gdrive.Service, error) {
	return d.source.Pool.Next(ctx)
}

// ---- fs.FileInfo / fs.DirEntry ----

type driveFileInfo struct{ it Item }

func (i *driveFileInfo) Name() string { return i.it.Name }
func (i *driveFileInfo) Size() int64  { return i.it.Size }
func (i *driveFileInfo) Mode() fs.FileMode {
	if i.it.IsFolder {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i *driveFileInfo) ModTime() time.Time { return i.it.ModTime }
func (i *driveFileInfo) IsDir() bool         { return i.it.IsFolder }
func (i *driveFileInfo) Sys() any            { return i.it }

// driveDirEntry adapts an Item to fs.DirEntry.
type driveDirEntry struct{ it Item }

func (e *driveDirEntry) Name() string { return e.it.Name }
func (e *driveDirEntry) IsDir() bool  { return e.it.IsFolder }
func (e *driveDirEntry) Type() fs.FileMode {
	if e.it.IsFolder {
		return fs.ModeDir
	}
	return 0
}
func (e *driveDirEntry) Info() (fs.FileInfo, error) { return &driveFileInfo{it: e.it}, nil }

// ---- file handle (fs.ReadDirFile + io.ReadSeeker) ----

type driveFile struct {
	fsys *DriveFS
	it   Item

	// content stream state (files only)
	offset int64
	rc     io.ReadCloser
	rcAt   int64 // offset at which rc was opened

	// directory iteration state (folders only)
	dirEntries []fs.DirEntry
	dirRead    bool
	dirPos     int
}

func (f *driveFile) Stat() (fs.FileInfo, error) { return &driveFileInfo{it: f.it}, nil }

func (f *driveFile) ensureStream() error {
	if f.rc != nil && f.rcAt == f.offset {
		return nil
	}
	if f.rc != nil {
		f.rc.Close()
		f.rc = nil
	}
	ctx := context.Background()
	svc, err := f.fsys.service(ctx)
	if err != nil {
		return err
	}
	rc, err := OpenRange(ctx, svc, f.it.ID, f.offset)
	if err != nil {
		return err
	}
	f.rc = rc
	f.rcAt = f.offset
	return nil
}

func (f *driveFile) Read(p []byte) (int, error) {
	if f.it.IsFolder {
		return 0, &fs.PathError{Op: "read", Path: f.it.Name, Err: errors.New("is a directory")}
	}
	if f.offset >= f.it.Size && f.it.Size > 0 {
		return 0, io.EOF
	}
	if err := f.ensureStream(); err != nil {
		return 0, err
	}

	// Greedily fill p. Network streams return short reads, but callers such as
	// oshash issue a single Read expecting an os.File-style full buffer, so we
	// loop until p is full or the stream ends.
	total := 0
	for total < len(p) {
		n, err := f.rc.Read(p[total:])
		total += n
		f.offset += int64(n)
		f.rcAt = f.offset
		if err != nil {
			if err == io.EOF && total > 0 {
				return total, nil
			}
			return total, err
		}
	}
	return total, nil
}

// Seek repositions the read offset; the next Read lazily opens a fresh ranged
// stream. This is what makes oshash's tail read cheap.
func (f *driveFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.offset + offset
	case io.SeekEnd:
		abs = f.it.Size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	if abs != f.offset {
		f.offset = abs
		if f.rc != nil {
			f.rc.Close()
			f.rc = nil
		}
	}
	return f.offset, nil
}

func (f *driveFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !f.it.IsFolder {
		return nil, &fs.PathError{Op: "readdir", Path: f.it.Name, Err: errors.New("not a directory")}
	}
	if !f.dirRead {
		// Lazily fetch this folder's children from the Drive API the first time
		// it is visited (caching into the index). This lets the scanner create
		// scenes as it walks, instead of waiting for a full pre-enumeration.
		listed, err := f.fsys.source.Index.IsListed(f.it.ID)
		if err != nil {
			return nil, err
		}
		if !listed {
			ctx := context.Background()
			svc, err := f.fsys.service(ctx)
			if err != nil {
				return nil, err
			}
			if err := ListFolder(ctx, svc, f.fsys.source.DriveID, f.it.ID, func(batch []Item) error {
				return f.fsys.source.Index.Upsert(batch)
			}); err != nil {
				return nil, err
			}
			if err := f.fsys.source.Index.MarkListed(f.it.ID); err != nil {
				return nil, err
			}
		}

		children, err := f.fsys.source.Index.Children(f.it.ID)
		if err != nil {
			return nil, err
		}
		f.dirEntries = make([]fs.DirEntry, len(children))
		for i, c := range children {
			f.dirEntries[i] = &driveDirEntry{it: c}
		}
		f.dirRead = true
	}

	if n <= 0 {
		rest := f.dirEntries[f.dirPos:]
		f.dirPos = len(f.dirEntries)
		return rest, nil
	}

	if f.dirPos >= len(f.dirEntries) {
		return nil, io.EOF
	}
	end := f.dirPos + n
	if end > len(f.dirEntries) {
		end = len(f.dirEntries)
	}
	batch := f.dirEntries[f.dirPos:end]
	f.dirPos = end
	return batch, nil
}

func (f *driveFile) Close() error {
	if f.rc != nil {
		err := f.rc.Close()
		f.rc = nil
		return err
	}
	return nil
}

// ensure interface satisfaction at compile time.
var (
	_ models.FS       = (*DriveFS)(nil)
	_ fs.ReadDirFile  = (*driveFile)(nil)
	_ io.ReadSeeker   = (*driveFile)(nil)
	_ fs.FileInfo     = (*driveFileInfo)(nil)
	_ fs.DirEntry     = (*driveDirEntry)(nil)
)

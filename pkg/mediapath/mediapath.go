// Package mediapath provides a process-wide hook for resolving a (possibly
// virtual) media path to a real local file path that ffmpeg and http.ServeFile
// can consume. For Google Drive sources this triggers an on-demand cache
// download; for local files it is a no-op. The hook lives in this leaf package
// so the low-level ffmpeg/generation code can call it without importing the
// manager (avoiding an import cycle).
package mediapath

// Resolver, if set, maps a media path to a local file path, fetching it into a
// local cache when necessary. It is installed by the manager at startup.
var Resolver func(path string) (string, error)

// Resolve returns a local path for the given media path. When no resolver is
// installed, or the path is already local, the input is returned unchanged.
func Resolve(path string) (string, error) {
	if Resolver == nil {
		return path, nil
	}
	return Resolver(path)
}

// ProbeResolver, if set, maps a media path to an authenticated input URL plus
// HTTP headers that ffprobe/ffmpeg can read with range requests. ok is false
// for local (or unmanaged) paths, which should be probed by their path
// directly. This lets metadata probing avoid full downloads for remote files.
var ProbeResolver func(path string) (url string, headers []string, ok bool, err error)

// ProbeTarget returns a ranged-readable ffprobe input for the given path.
// ok is false when the path should be probed directly (local files).
func ProbeTarget(path string) (url string, headers []string, ok bool, err error) {
	if ProbeResolver == nil {
		return "", nil, false, nil
	}
	return ProbeResolver(path)
}

// HeadReader, if set, returns the first n bytes of a media path. ok is false for
// local/unmanaged paths (read them directly). Used for cheap container magic-byte
// detection on remote files without a full download.
var HeadReader func(path string, n int) (data []byte, ok bool, err error)

// ReadHead returns the first n bytes of path via the installed reader. ok is
// false when the path should be read directly from disk.
func ReadHead(path string, n int) (data []byte, ok bool, err error) {
	if HeadReader == nil {
		return nil, false, nil
	}
	return HeadReader(path, n)
}

// ThumbResolver, if set, returns a native thumbnail image (JPEG bytes) for a
// media path, optionally resized to `size` px. ok is false when no native
// thumbnail is available (caller should generate one itself). Lets covers and
// image thumbnails come straight from Drive without downloading the full file.
var ThumbResolver func(path string, size int) (data []byte, ok bool, err error)

// ThumbData returns a native thumbnail for path. ok is false when the caller
// should fall back to generating the thumbnail.
func ThumbData(path string, size int) (data []byte, ok bool, err error) {
	if ThumbResolver == nil {
		return nil, false, nil
	}
	return ThumbResolver(path, size)
}

// Meta holds native media metadata a backend can provide without reading the
// file (e.g. Google Drive's videoMediaMetadata/imageMediaMetadata).
type Meta struct {
	Width      int64
	Height     int64
	DurationMS int64
}

// MetaResolver, if set, returns native metadata for a path. ok is false when no
// native metadata is available (caller should probe the file itself).
var MetaResolver func(path string) (Meta, bool, error)

// MediaMeta returns native metadata for path.
func MediaMeta(path string) (Meta, bool, error) {
	if MetaResolver == nil {
		return Meta{}, false, nil
	}
	return MetaResolver(path)
}

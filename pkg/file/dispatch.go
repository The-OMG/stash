package file

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/stashapp/stash/pkg/models"
)

// mount maps a virtual path prefix to a backing FS.
type mount struct {
	prefix  string
	backend models.FS
}

// DispatchFS routes models.FS operations to a registered backend based on path
// prefix, falling back to OsFS for unregistered (local) paths. This lets Google
// Drive sources and local libraries coexist transparently throughout the
// scanner and serving code: a single FS value handles every path.
type DispatchFS struct {
	mu     sync.RWMutex
	mounts []mount
	osfs   OsFS
}

var defaultDispatch = &DispatchFS{}

// DefaultFS returns the process-wide dispatching file system. Use this in place
// of &OsFS{} wherever a models.FS is needed so Drive-backed paths are routed
// automatically.
func DefaultFS() *DispatchFS { return defaultDispatch }

// Mount registers backend for all paths at or under prefix. Longer prefixes
// take precedence, so nested mounts resolve correctly.
func (d *DispatchFS) Mount(prefix string, backend models.FS) {
	prefix = filepath.Clean(prefix)
	d.mu.Lock()
	defer d.mu.Unlock()

	for i := range d.mounts {
		if d.mounts[i].prefix == prefix {
			d.mounts[i].backend = backend
			return
		}
	}
	d.mounts = append(d.mounts, mount{prefix: prefix, backend: backend})
	sort.Slice(d.mounts, func(i, j int) bool {
		return len(d.mounts[i].prefix) > len(d.mounts[j].prefix)
	})
}

// Unmount removes the backend registered at prefix.
func (d *DispatchFS) Unmount(prefix string) {
	prefix = filepath.Clean(prefix)
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.mounts {
		if d.mounts[i].prefix == prefix {
			d.mounts = append(d.mounts[:i], d.mounts[i+1:]...)
			return
		}
	}
}

// IsManaged reports whether name is served by a registered (non-OS) backend.
// Used by media code to decide whether a path must be resolved via cache.
func (d *DispatchFS) IsManaged(name string) bool {
	clean := filepath.Clean(name)
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, m := range d.mounts {
		if clean == m.prefix || strings.HasPrefix(clean, m.prefix+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// backendFor returns the FS responsible for name.
func (d *DispatchFS) backendFor(name string) models.FS {
	clean := filepath.Clean(name)
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, m := range d.mounts {
		if clean == m.prefix || strings.HasPrefix(clean, m.prefix+string(filepath.Separator)) {
			return m.backend
		}
	}
	return &d.osfs
}

func (d *DispatchFS) Stat(name string) (fs.FileInfo, error)  { return d.backendFor(name).Stat(name) }
func (d *DispatchFS) Lstat(name string) (fs.FileInfo, error) { return d.backendFor(name).Lstat(name) }
func (d *DispatchFS) Open(name string) (fs.ReadDirFile, error) {
	return d.backendFor(name).Open(name)
}
func (d *DispatchFS) OpenZip(name string, size int64) (models.ZipFS, error) {
	return d.backendFor(name).OpenZip(name, size)
}
func (d *DispatchFS) IsPathCaseSensitive(path string) (bool, error) {
	return d.backendFor(path).IsPathCaseSensitive(path)
}

var _ models.FS = (*DispatchFS)(nil)

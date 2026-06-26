package drive

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Cache is a size-bounded local disk cache of Drive file content. Media
// operations (ffmpeg transcode/generate, direct play) require a real seekable
// file, so the file is downloaded on demand and evicted LRU-style when the
// cache exceeds its size limit.
type Cache struct {
	dir      string
	maxBytes int64

	keyed   sync.Map // fileID -> *sync.Mutex, to dedupe concurrent fetches
	evictMu sync.Mutex
}

// NewCache creates (and ensures) a cache rooted at dir, capped at maxBytes
// (0 = unbounded).
func NewCache(dir string, maxBytes int64) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Cache{dir: dir, maxBytes: maxBytes}, nil
}

func (c *Cache) path(fileID string) string { return filepath.Join(c.dir, fileID) }

func (c *Cache) lockFor(id string) *sync.Mutex {
	m, _ := c.keyed.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// EnsureLocal returns the local path of fileID's content, downloading it first
// if absent or stale. Concurrent calls for the same id are serialized; distinct
// ids download in parallel.
func (c *Cache) EnsureLocal(ctx context.Context, pool *SAPool, fileID string, size int64) (string, error) {
	mu := c.lockFor(fileID)
	mu.Lock()
	defer mu.Unlock()

	lp := c.path(fileID)
	if fi, err := os.Stat(lp); err == nil && (size == 0 || fi.Size() == size) {
		now := time.Now()
		_ = os.Chtimes(lp, now, now) // touch for LRU recency
		return lp, nil
	}

	svc, err := pool.Next(ctx)
	if err != nil {
		return "", err
	}
	rc, err := OpenRange(ctx, svc, fileID, 0)
	if err != nil {
		return "", err
	}
	defer rc.Close()

	tmp := lp + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, lp); err != nil {
		os.Remove(tmp)
		return "", err
	}

	c.evict()
	return lp, nil
}

// evict removes least-recently-used entries until total size <= maxBytes.
func (c *Cache) evict() {
	if c.maxBytes <= 0 {
		return
	}
	c.evictMu.Lock()
	defer c.evictMu.Unlock()

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}

	type fe struct {
		path string
		size int64
		mod  time.Time
	}
	var files []fe
	var total int64
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) == ".tmp" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fe{filepath.Join(c.dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= c.maxBytes {
		return
	}

	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= c.maxBytes {
			break
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
	}
}

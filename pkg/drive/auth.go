// Package drive provides native Google Drive integration for stash: a
// service-account credential pool, shared-drive listing and incremental
// change detection (Drive Changes API), and a models.FS implementation so
// scanning operates directly against Drive instead of a FUSE mount.
package drive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// DefaultScope grants full Drive access. Use drive.DriveReadonlyScope for
// read-only sources.
const DefaultScope = drive.DriveScope

// SAPool holds one or more service-account credentials and hands out
// *drive.Service instances, rotating across accounts to spread API quota
// across the project's per-100s limits. A single shared drive listing is
// cheap (~1 call per 1000 files) so rotation mainly benefits parallel media
// downloads and rate-limit recovery.
type SAPool struct {
	scope string
	files []string
	next  uint64

	mu    sync.Mutex
	cache map[string]*drive.Service
}

// NewSAPool builds a pool from a path that is either a single service-account
// JSON file or a directory containing many of them (e.g. an rclone gdsa key
// directory).
func NewSAPool(path, scope string) (*SAPool, error) {
	if scope == "" {
		scope = DefaultScope
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("service account path %q: %w", path, err)
	}

	var files []string
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			files = append(files, filepath.Join(path, e.Name()))
		}
	} else {
		files = []string{path}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no service account json files found at %q", path)
	}

	return &SAPool{
		scope: scope,
		files: files,
		cache: make(map[string]*drive.Service),
	}, nil
}

// Len returns the number of service accounts in the pool.
func (p *SAPool) Len() int { return len(p.files) }

// serviceFor builds (and caches) a *drive.Service for a specific SA file.
func (p *SAPool) serviceFor(ctx context.Context, file string) (*drive.Service, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if svc, ok := p.cache[file]; ok {
		return svc, nil
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	creds, err := google.CredentialsFromJSON(ctx, data, p.scope)
	if err != nil {
		return nil, fmt.Errorf("parse service account %s: %w", filepath.Base(file), err)
	}

	svc, err := drive.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, err
	}

	p.cache[file] = svc
	return svc, nil
}

// Next returns the next service in round-robin rotation. Safe for concurrent
// use.
func (p *SAPool) Next(ctx context.Context) (*drive.Service, error) {
	i := atomic.AddUint64(&p.next, 1)
	return p.serviceFor(ctx, p.files[int(i)%len(p.files)])
}

// First returns a stable service (the first SA) for operations that should
// not rotate mid-flight, such as paginated listings and change polls.
func (p *SAPool) First(ctx context.Context) (*drive.Service, error) {
	return p.serviceFor(ctx, p.files[0])
}

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stashapp/stash/pkg/drive"
	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/mediapath"
)

// defaultDriveCacheBytes caps each source's on-demand media cache (50 GiB).
const defaultDriveCacheBytes int64 = 50 << 30

// driveVirtualRoot is the reserved path prefix under which Google Drive sources
// are mounted in the dispatching file system. Stored file paths look like
// /__gdrive__/<sourceID>/<relative/path>. These paths never touch the OS; the
// dispatcher routes them to a DriveFS.
const driveVirtualRoot = "/__gdrive__"

// driveSourcesFile is the sidecar config (under the stash config directory)
// describing native Drive sources. A full GraphQL/UI config comes later; this
// bootstraps the engine in the meantime.
const driveSourcesFile = "gdrive_sources.json"

// driveSourceConfig is one configured Google Drive source.
type driveSourceConfig struct {
	ID       string `json:"id"`        // stable identifier; used in the virtual path
	Name     string `json:"name"`      // display name
	DriveID  string `json:"drive_id"`  // shared (team) drive id
	KeysPath string `json:"keys_path"` // service-account json file or directory
	Scope    string `json:"scope"`     // optional oauth scope (default full drive)
	CacheDir   string `json:"cache_dir"`         // optional media cache dir (default under cache path)
	CacheBytes int64  `json:"cache_bytes"`       // optional cache size cap (default 50 GiB)
}

type driveSourcesConfig struct {
	Sources []driveSourceConfig `json:"sources"`
}

// managedDriveSource is a live, mounted Drive source.
type managedDriveSource struct {
	cfg    driveSourceConfig
	source *drive.Source
	fs     *drive.DriveFS
	cache  *drive.Cache
	root   string // virtual library root, e.g. /__gdrive__/<id>
}

// loadDriveSourcesConfig reads the sidecar config; a missing file is not an
// error (just no sources).
func (s *Manager) loadDriveSourcesConfig() (driveSourcesConfig, error) {
	path := filepath.Join(s.Config.GetConfigPath(), driveSourcesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return driveSourcesConfig{}, nil
		}
		return driveSourcesConfig{}, err
	}
	var cfg driveSourcesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return driveSourcesConfig{}, fmt.Errorf("parsing %s: %w", driveSourcesFile, err)
	}
	return cfg, nil
}

// RefreshDriveSources (re)builds and mounts all configured Drive sources. It is
// safe to call repeatedly: existing mounts are replaced.
func (s *Manager) RefreshDriveSources(ctx context.Context) {
	cfg, err := s.loadDriveSourcesConfig()
	if err != nil {
		logger.Errorf("drive: could not load %s: %v", driveSourcesFile, err)
		return
	}

	// unmount previously registered roots
	for _, ms := range s.driveSources {
		file.DefaultFS().Unmount(ms.root)
		if ms.source != nil && ms.source.Index != nil {
			ms.source.Index.Close()
		}
	}
	s.driveSources = nil

	indexDir := filepath.Join(s.Config.GetConfigPath(), "gdrive")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		logger.Errorf("drive: could not create index dir %s: %v", indexDir, err)
		return
	}

	for _, sc := range cfg.Sources {
		if sc.ID == "" || sc.DriveID == "" || sc.KeysPath == "" {
			logger.Warnf("drive: skipping source with missing id/drive_id/keys_path: %+v", sc)
			continue
		}

		pool, err := drive.NewSAPool(sc.KeysPath, sc.Scope)
		if err != nil {
			logger.Errorf("drive[%s]: auth: %v", sc.ID, err)
			continue
		}

		idxPath := filepath.Join(indexDir, sc.ID+".sqlite")
		index, err := drive.OpenIndex(idxPath, sc.DriveID)
		if err != nil {
			logger.Errorf("drive[%s]: open index: %v", sc.ID, err)
			continue
		}

		source := &drive.Source{DriveID: sc.DriveID, Pool: pool, Index: index}
		root := filepath.Join(driveVirtualRoot, sc.ID)
		dfs := drive.NewDriveFS(source, root)

		cacheDir := sc.CacheDir
		if cacheDir == "" {
			cacheDir = filepath.Join(s.Config.GetCachePath(), "gdrive", sc.ID)
		}
		cacheBytes := sc.CacheBytes
		if cacheBytes == 0 {
			cacheBytes = defaultDriveCacheBytes
		}
		cache, err := drive.NewCache(cacheDir, cacheBytes)
		if err != nil {
			logger.Errorf("drive[%s]: open cache: %v", sc.ID, err)
			index.Close()
			continue
		}

		file.DefaultFS().Mount(root, dfs)
		s.driveSources = append(s.driveSources, &managedDriveSource{
			cfg: sc, source: source, fs: dfs, cache: cache, root: root,
		})
		logger.Infof("drive[%s]: mounted shared drive %s at %s (%d service accounts, %s)",
			sc.ID, sc.DriveID, root, pool.Len(), index.String())
	}

	// install the media path resolver so ffmpeg/playback fetch Drive files into
	// the local cache on demand.
	mediapath.Resolver = s.resolveMediaPath
}

// resolveMediaPath maps a virtual Drive path to a local cached file path,
// downloading on demand. Non-Drive (local) paths are returned unchanged.
func (s *Manager) resolveMediaPath(path string) (string, error) {
	clean := filepath.Clean(path)
	for _, ms := range s.driveSources {
		if clean != ms.root && !strings.HasPrefix(clean, ms.root+string(filepath.Separator)) {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(clean, ms.root), string(filepath.Separator))
		it, ok, err := ms.source.Index.LookupPath(rel)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("drive[%s]: path not in index: %s", ms.cfg.ID, rel)
		}
		return ms.cache.EnsureLocal(context.Background(), ms.source.Pool, it.ID, it.Size)
	}
	return path, nil
}

// DriveRoots returns the virtual library roots of all mounted Drive sources,
// to be appended to the scanner's root paths.
func (s *Manager) DriveRoots() []string {
	roots := make([]string, 0, len(s.driveSources))
	for _, ms := range s.driveSources {
		roots = append(roots, ms.root)
	}
	return roots
}

// SyncDriveSources refreshes each Drive index from the Drive API: a cold full
// list on first run, then fast incremental Changes-API polls. Called before a
// scan so the dispatcher's view of each drive is current.
func (s *Manager) SyncDriveSources(ctx context.Context) {
	for _, ms := range s.driveSources {
		stats, err := ms.source.Sync(ctx)
		if err != nil {
			logger.Errorf("drive[%s]: sync: %v", ms.cfg.ID, err)
			continue
		}
		if stats.Full {
			logger.Infof("drive[%s]: full index complete: %d files", ms.cfg.ID, stats.Total)
		} else {
			logger.Infof("drive[%s]: incremental sync: %d updated, %d removed (%d total)",
				ms.cfg.ID, stats.Updated, stats.Removed, stats.Total)
		}
	}
}

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

// DriveSourceStatus is the manager-level (exported) view of a configured Drive
// source, for the GraphQL layer.
type DriveSourceStatus struct {
	ID        string
	Name      string
	DriveID   string
	KeysPath  string
	Scope     string
	CacheDir  string
	FileCount int
	Mounted   bool
}

// DriveSourceParams is the input for adding/replacing a Drive source.
type DriveSourceParams struct {
	ID         string
	Name       string
	DriveID    string
	KeysPath   string
	Scope      string
	CacheDir   string
	CacheBytes int64
}

// ListDriveSources returns the configured sources (from the sidecar) annotated
// with live mount/index status.
func (s *Manager) ListDriveSources() []DriveSourceStatus {
	cfg, err := s.loadDriveSourcesConfig()
	if err != nil {
		logger.Errorf("drive: list: %v", err)
		return nil
	}
	mounted := make(map[string]*managedDriveSource, len(s.driveSources))
	for _, ms := range s.driveSources {
		mounted[ms.cfg.ID] = ms
	}

	out := make([]DriveSourceStatus, 0, len(cfg.Sources))
	for _, sc := range cfg.Sources {
		st := DriveSourceStatus{
			ID: sc.ID, Name: sc.Name, DriveID: sc.DriveID,
			KeysPath: sc.KeysPath, Scope: sc.Scope, CacheDir: sc.CacheDir,
		}
		if ms, ok := mounted[sc.ID]; ok {
			st.Mounted = true
			if n, err := ms.source.Index.Count(); err == nil {
				st.FileCount = n
			}
		}
		out = append(out, st)
	}
	return out
}

func (s *Manager) saveDriveSourcesConfig(cfg driveSourcesConfig) error {
	path := filepath.Join(s.Config.GetConfigPath(), driveSourcesFile)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// AddDriveSource validates Drive access, persists the source to the sidecar
// config (replacing any existing source with the same id), and remounts.
func (s *Manager) AddDriveSource(ctx context.Context, in DriveSourceParams) error {
	if in.ID == "" || in.DriveID == "" || in.KeysPath == "" {
		return fmt.Errorf("id, drive_id and keys_path are required")
	}

	// validate auth + drive access up front so the UI gets immediate feedback.
	pool, err := drive.NewSAPool(in.KeysPath, in.Scope)
	if err != nil {
		return fmt.Errorf("service account: %w", err)
	}
	svc, err := pool.First(ctx)
	if err != nil {
		return err
	}
	if _, err := drive.StartPageToken(ctx, svc, in.DriveID); err != nil {
		return fmt.Errorf("cannot access shared drive %s: %w", in.DriveID, err)
	}

	cfg, err := s.loadDriveSourcesConfig()
	if err != nil {
		return err
	}
	sc := driveSourceConfig{
		ID: in.ID, Name: in.Name, DriveID: in.DriveID, KeysPath: in.KeysPath,
		Scope: in.Scope, CacheDir: in.CacheDir, CacheBytes: in.CacheBytes,
	}
	replaced := false
	for i := range cfg.Sources {
		if cfg.Sources[i].ID == in.ID {
			cfg.Sources[i] = sc
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Sources = append(cfg.Sources, sc)
	}
	if err := s.saveDriveSourcesConfig(cfg); err != nil {
		return err
	}

	s.RefreshDriveSources(ctx)
	return nil
}

// RemoveDriveSource removes a source from the sidecar config and unmounts it.
// The on-disk index and cache are left in place.
func (s *Manager) RemoveDriveSource(ctx context.Context, id string) error {
	cfg, err := s.loadDriveSourcesConfig()
	if err != nil {
		return err
	}
	kept := make([]driveSourceConfig, 0, len(cfg.Sources))
	found := false
	for _, sc := range cfg.Sources {
		if sc.ID == id {
			found = true
			continue
		}
		kept = append(kept, sc)
	}
	if !found {
		return fmt.Errorf("no drive source with id %q", id)
	}
	cfg.Sources = kept
	if err := s.saveDriveSourcesConfig(cfg); err != nil {
		return err
	}

	s.RefreshDriveSources(ctx)
	return nil
}

// SyncDriveSourceByID triggers a background index sync for one mounted source.
func (s *Manager) SyncDriveSourceByID(id string) error {
	for _, ms := range s.driveSources {
		if ms.cfg.ID == id {
			go func(m *managedDriveSource) {
				if _, err := m.source.Sync(context.Background()); err != nil {
					logger.Errorf("drive[%s]: background sync: %v", m.cfg.ID, err)
				}
			}(ms)
			return nil
		}
	}
	return fmt.Errorf("no mounted drive source with id %q", id)
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

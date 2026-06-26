package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	ID           string `json:"id"`             // stable identifier; used in the virtual path
	Name         string `json:"name"`           // display name
	DriveID      string `json:"drive_id"`       // shared (team) drive id
	RootFolderID string `json:"root_folder_id"` // optional: scope to a folder instead of the whole drive
	KeysPath     string `json:"keys_path"`      // service-account json file or directory
	Scope        string `json:"scope"`          // optional oauth scope (default full drive)
	CacheDir     string `json:"cache_dir"`      // optional media cache dir (default under cache path)
	CacheBytes   int64  `json:"cache_bytes"`    // optional cache size cap (default 50 GiB)

	// auth: defaults to service-account (keys_path). Alternatively OAuth, or
	// import everything (token + ids) from an existing rclone remote.
	AuthType     string `json:"auth_type"`     // "sa" (default) | "oauth"
	ClientID     string `json:"client_id"`     // oauth client id (rclone default if empty)
	ClientSecret string `json:"client_secret"` // oauth client secret
	Token        string `json:"token"`         // oauth token JSON (rclone format)
	RcloneRemote string `json:"rclone_remote"` // import token/ids from this rclone remote
}

// buildSourceAuth constructs the Authenticator for a source and, when importing
// an rclone remote, fills in drive_id/root_folder_id from it if unset.
func (s *Manager) buildSourceAuth(ctx context.Context, sc *driveSourceConfig) (drive.Authenticator, error) {
	if sc.RcloneRemote != "" {
		r, err := drive.ResolveRcloneRemote(sc.RcloneRemote)
		if err != nil {
			return nil, err
		}
		if sc.DriveID == "" {
			sc.DriveID = r.TeamDrive
		}
		if sc.RootFolderID == "" {
			sc.RootFolderID = r.RootFolderID
		}
		return r.Authenticator(ctx, sc.Scope)
	}
	if sc.AuthType == "oauth" || sc.Token != "" {
		return drive.NewOAuthSourceFromRcloneToken(ctx, sc.Token, sc.ClientID, sc.ClientSecret, sc.Scope)
	}
	return drive.NewSAPool(sc.KeysPath, sc.Scope)
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
		if sc.ID == "" || (sc.DriveID == "" && sc.RcloneRemote == "") {
			logger.Warnf("drive: skipping source missing id or drive_id/rclone_remote: %+v", sc)
			continue
		}

		// buildSourceAuth may fill sc.DriveID/RootFolderID from an rclone remote.
		auth, err := s.buildSourceAuth(ctx, &sc)
		if err != nil {
			logger.Errorf("drive[%s]: auth: %v", sc.ID, err)
			continue
		}
		if sc.DriveID == "" {
			logger.Errorf("drive[%s]: no drive id (set drive_id or use an rclone remote with team_drive)", sc.ID)
			continue
		}

		idxPath := filepath.Join(indexDir, sc.ID+".sqlite")
		index, err := drive.OpenIndex(idxPath, sc.DriveID, sc.RootFolderID)
		if err != nil {
			logger.Errorf("drive[%s]: open index: %v", sc.ID, err)
			continue
		}

		source := &drive.Source{DriveID: sc.DriveID, Pool: auth, Index: index}
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
		logger.Infof("drive[%s]: mounted shared drive %s at %s (%d account(s), %s)",
			sc.ID, sc.DriveID, root, auth.Len(), index.String())
	}

	// install the media path resolvers:
	//  - Resolver: full local cache download for content processing (playback,
	//    transcode, sprites, previews) which need a seekable whole file.
	//  - ProbeResolver: authenticated ranged URL for metadata probing (ffprobe
	//    during scan), so indexing does not download whole files.
	mediapath.Resolver = s.resolveMediaPath
	mediapath.ProbeResolver = s.resolveProbeTarget
	mediapath.HeadReader = s.resolveHead
	mediapath.ThumbResolver = s.resolveThumb
	mediapath.MetaResolver = s.resolveMeta

	// route deletion of Drive-backed files to the drive's trash (reversible).
	file.DriveTrasher = s
}

// resolveMeta returns Drive's native dimensions/duration for a path so the
// scanner can skip ffprobe where possible. ok is false for local paths.
func (s *Manager) resolveMeta(path string) (mediapath.Meta, bool, error) {
	ms, rel, ok := s.driveSourceForPath(path)
	if !ok {
		return mediapath.Meta{}, false, nil
	}
	it, found, err := ms.source.Index.LookupPath(rel)
	if err != nil || !found {
		return mediapath.Meta{}, false, err
	}
	return mediapath.Meta{Width: it.Width, Height: it.Height, DurationMS: it.DurationMS}, true, nil
}

// StreamDriveDirect proxies a direct-play request for a Drive-backed scene
// straight from Drive (honoring the browser's Range header), without
// downloading the whole file to cache. Returns false if path is not a mounted
// Drive path (caller should fall back to local/cache serving).
func (s *Manager) StreamDriveDirect(w http.ResponseWriter, r *http.Request, path string) bool {
	ms, rel, ok := s.driveSourceForPath(path)
	if !ok {
		return false
	}
	it, found, err := ms.source.Index.LookupPath(rel)
	if err != nil || !found {
		return false
	}

	resp, err := ms.source.StreamRange(r.Context(), it.ID, r.Header.Get("Range"))
	if err != nil {
		logger.Errorf("drive[%s]: stream %s: %v", ms.cfg.ID, rel, err)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return true
	}
	defer resp.Body.Close()

	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		h.Set("Content-Type", ct)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		h.Set("Content-Range", cr)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		h.Set("Content-Length", cl)
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
	return true
}

// driveSourceForPath returns the mounted source owning path and the
// drive-relative path under it.
func (s *Manager) driveSourceForPath(path string) (*managedDriveSource, string, bool) {
	clean := filepath.Clean(path)
	for _, ms := range s.driveSources {
		if clean == ms.root || strings.HasPrefix(clean, ms.root+string(filepath.Separator)) {
			rel := strings.TrimPrefix(strings.TrimPrefix(clean, ms.root), string(filepath.Separator))
			return ms, rel, true
		}
	}
	return nil, "", false
}

// IsManaged reports whether path belongs to a mounted Drive source.
func (s *Manager) IsManaged(path string) bool {
	_, _, ok := s.driveSourceForPath(path)
	return ok
}

// Trash moves a Drive-backed file to the drive's trash (file.RemoteTrasher).
func (s *Manager) Trash(path string) error {
	ms, rel, ok := s.driveSourceForPath(path)
	if !ok {
		return fmt.Errorf("not a drive path: %s", path)
	}
	it, found, err := ms.source.Index.LookupPath(rel)
	if err != nil {
		return err
	}
	if !found {
		return nil // already gone
	}
	return ms.source.SetTrashed(context.Background(), it.ID, true)
}

// Untrash restores a previously-trashed Drive file (delete rollback).
func (s *Manager) Untrash(path string) error {
	ms, rel, ok := s.driveSourceForPath(path)
	if !ok {
		return fmt.Errorf("not a drive path: %s", path)
	}
	it, found, err := ms.source.Index.LookupPath(rel)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	return ms.source.SetTrashed(context.Background(), it.ID, false)
}

// resolveThumb returns Drive's own thumbnail (JPEG bytes) for a Drive-backed
// path, so covers/image thumbnails skip the full download + ffmpeg/vips. ok is
// false for local paths or when Drive has no thumbnail (caller generates one).
func (s *Manager) resolveThumb(path string, size int) ([]byte, bool, error) {
	clean := filepath.Clean(path)
	for _, ms := range s.driveSources {
		if clean != ms.root && !strings.HasPrefix(clean, ms.root+string(filepath.Separator)) {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(clean, ms.root), string(filepath.Separator))
		it, ok, err := ms.source.Index.LookupPath(rel)
		if err != nil {
			return nil, false, err
		}
		if !ok || !it.HasThumbnail {
			return nil, false, nil // no native thumbnail -> caller falls back
		}
		data, err := ms.source.ThumbnailData(context.Background(), it.ID, size)
		if err != nil {
			return nil, false, err
		}
		return data, true, nil
	}
	return nil, false, nil
}

// resolveHead returns the first n bytes of a Drive-backed path via a ranged GET
// (for container magic-byte detection). ok is false for local paths.
func (s *Manager) resolveHead(path string, n int) ([]byte, bool, error) {
	clean := filepath.Clean(path)
	for _, ms := range s.driveSources {
		if clean != ms.root && !strings.HasPrefix(clean, ms.root+string(filepath.Separator)) {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(clean, ms.root), string(filepath.Separator))
		it, ok, err := ms.source.Index.LookupPath(rel)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, fmt.Errorf("drive[%s]: path not in index: %s", ms.cfg.ID, rel)
		}
		data, err := ms.source.ReadHead(context.Background(), it.ID, n)
		if err != nil {
			return nil, false, err
		}
		return data, true, nil
	}
	return nil, false, nil
}

// resolveProbeTarget maps a virtual Drive path to an authenticated, ranged
// ffprobe input URL + headers. ok is false for local paths (probe by path).
func (s *Manager) resolveProbeTarget(path string) (string, []string, bool, error) {
	clean := filepath.Clean(path)
	for _, ms := range s.driveSources {
		if clean != ms.root && !strings.HasPrefix(clean, ms.root+string(filepath.Separator)) {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(clean, ms.root), string(filepath.Separator))
		it, ok, err := ms.source.Index.LookupPath(rel)
		if err != nil {
			return "", nil, false, err
		}
		if !ok {
			return "", nil, false, fmt.Errorf("drive[%s]: path not in index: %s", ms.cfg.ID, rel)
		}
		url, headers, err := ms.source.ProbeTarget(context.Background(), it.ID)
		if err != nil {
			return "", nil, false, err
		}
		return url, headers, true, nil
	}
	return "", nil, false, nil
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
	ID           string
	Name         string
	DriveID      string
	RootFolderID string
	KeysPath     string
	Scope        string
	CacheDir     string
	FileCount    int
	Mounted      bool
}

// DriveSourceParams is the input for adding/replacing a Drive source.
type DriveSourceParams struct {
	ID           string
	Name         string
	DriveID      string
	RootFolderID string
	KeysPath     string
	Scope        string
	CacheDir     string
	CacheBytes   int64
	AuthType     string
	ClientID     string
	ClientSecret string
	Token        string
	RcloneRemote string
}

// DriveFolderInfo is a folder returned by the source picker.
type DriveFolderInfo struct {
	ID   string
	Name string
}

// RcloneRemotes lists rclone remotes that resolve to a Drive (for the picker).
func (s *Manager) RcloneRemotes() ([]string, error) {
	return drive.ListRcloneDriveRemotes()
}

// BrowseDrive lists the folders directly under parentID (or the drive/source
// root when parentID is empty) using transient auth from the given params. Used
// by the source-picker UI to navigate without creating a source.
func (s *Manager) BrowseDrive(ctx context.Context, in DriveSourceParams, parentID string) ([]DriveFolderInfo, error) {
	sc := driveSourceConfig{
		DriveID: in.DriveID, RootFolderID: in.RootFolderID, KeysPath: in.KeysPath, Scope: in.Scope,
		AuthType: in.AuthType, ClientID: in.ClientID, ClientSecret: in.ClientSecret,
		Token: in.Token, RcloneRemote: in.RcloneRemote,
	}
	auth, err := s.buildSourceAuth(ctx, &sc)
	if err != nil {
		return nil, err
	}
	if sc.DriveID == "" {
		return nil, fmt.Errorf("no drive id resolved")
	}
	svc, err := auth.First(ctx)
	if err != nil {
		return nil, err
	}

	parent := parentID
	if parent == "" {
		if sc.RootFolderID != "" {
			parent = sc.RootFolderID
		} else {
			parent = sc.DriveID
		}
	}

	var folders []DriveFolderInfo
	err = drive.ListFolder(ctx, svc, sc.DriveID, parent, func(items []drive.Item) error {
		for _, it := range items {
			if it.IsFolder {
				folders = append(folders, DriveFolderInfo{ID: it.ID, Name: it.Name})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return folders, nil
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
			ID: sc.ID, Name: sc.Name, DriveID: sc.DriveID, RootFolderID: sc.RootFolderID,
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
	if in.ID == "" {
		return fmt.Errorf("id is required")
	}
	if in.DriveID == "" && in.RcloneRemote == "" {
		return fmt.Errorf("either drive_id or rclone_remote is required")
	}

	sc := driveSourceConfig{
		ID: in.ID, Name: in.Name, DriveID: in.DriveID, RootFolderID: in.RootFolderID,
		KeysPath: in.KeysPath, Scope: in.Scope, CacheDir: in.CacheDir, CacheBytes: in.CacheBytes,
		AuthType: in.AuthType, ClientID: in.ClientID, ClientSecret: in.ClientSecret,
		Token: in.Token, RcloneRemote: in.RcloneRemote,
	}

	// validate auth + drive access up front so the UI gets immediate feedback.
	// buildSourceAuth also fills DriveID/RootFolderID from an rclone remote.
	auth, err := s.buildSourceAuth(ctx, &sc)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if sc.DriveID == "" {
		return fmt.Errorf("no drive id resolved (set drive_id or use an rclone remote with a team_drive)")
	}
	svc, err := auth.First(ctx)
	if err != nil {
		return err
	}
	if _, err := drive.StartPageToken(ctx, svc, sc.DriveID); err != nil {
		return fmt.Errorf("cannot access shared drive %s: %w", sc.DriveID, err)
	}

	cfg, err := s.loadDriveSourcesConfig()
	if err != nil {
		return err
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

package manager

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/job"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
)

// MigrateStatus is the last (or in-progress) migration's state, surfaced to the
// UI so results display without staying on the settings page.
type MigrateStatus struct {
	Running    bool
	DriveID    string
	DryRun     bool
	Stats      MigrateStats
	FinishedAt *time.Time
}

var (
	migrateMu   sync.Mutex
	lastMigrate MigrateStatus
)

// GetMigrateStatus returns the most recent migration's status/result.
func (s *Manager) GetMigrateStatus() MigrateStatus {
	migrateMu.Lock()
	defer migrateMu.Unlock()
	return lastMigrate
}

func setMigrateRunning(driveID string, dryRun bool) {
	migrateMu.Lock()
	defer migrateMu.Unlock()
	lastMigrate = MigrateStatus{Running: true, DriveID: driveID, DryRun: dryRun}
}

func setMigrateDone(driveID string, dryRun bool, st MigrateStats) {
	migrateMu.Lock()
	defer migrateMu.Unlock()
	now := time.Now()
	lastMigrate = MigrateStatus{Running: false, DriveID: driveID, DryRun: dryRun, Stats: st, FinishedAt: &now}
}

// driveMigrateJob runs a path-based migration (dry-run or real) as a tracked,
// cancellable job, so it isn't tied to (and aborted by) the HTTP request.
type driveMigrateJob struct {
	mgr         *Manager
	prefix      string
	driveID     string
	driveName   string
	requireSize bool
	dryRun      bool
}

func (j *driveMigrateJob) Execute(ctx context.Context, progress *job.Progress) error {
	setMigrateRunning(j.driveID, j.dryRun)
	verb := "Migrating"
	if j.dryRun {
		verb = "Previewing migration of"
	}

	var scanned, migrated, total int64
	var stats MigrateStats
	done := make(chan error, 1)
	go func() {
		st, err := j.mgr.MigrateToDriveByPath(ctx, j.prefix, j.driveID, j.requireSize, j.dryRun,
			func(s, m, t int) {
				atomic.StoreInt64(&scanned, int64(s))
				atomic.StoreInt64(&migrated, int64(m))
				atomic.StoreInt64(&total, int64(t))
				if t > 0 {
					progress.SetTotal(t)
				}
				progress.SetProcessed(s)
			})
		stats = st
		done <- err
	}()

	for {
		var (
			loopErr error
			stop    bool
		)
		s, m, t := atomic.LoadInt64(&scanned), atomic.LoadInt64(&migrated), atomic.LoadInt64(&total)
		progress.ExecuteTask(
			fmt.Sprintf("%s %s — %d matched / %d checked of %d", verb, j.driveName, m, s, t),
			func() {
				select {
				case err := <-done:
					loopErr, stop = err, true
				case <-ctx.Done():
					loopErr, stop = ctx.Err(), true
				case <-time.After(time.Second):
				}
			})
		if stop {
			if loopErr != nil {
				return fmt.Errorf("drive-migrate[%s]: %w", j.driveID, loopErr)
			}
			setMigrateDone(j.driveID, j.dryRun, stats)
			matched := "repointed"
			if j.dryRun {
				matched = "would migrate"
			}
			summary := fmt.Sprintf("Drive %s %s — %d candidates, %d %s, %d not-in-db, %d size-mismatch, %d collision",
				map[bool]string{true: "DRY RUN", false: "migration"}[j.dryRun], j.driveName,
				stats.Candidates, stats.Migrated, matched, stats.NotInDB, stats.SizeMismatch, stats.Collision)
			progress.SetDescription(summary)
			logger.Infof("drive-migrate[%s]: %s", j.driveID, summary)
			return nil
		}
	}
}

// MigrateDriveByPathJob queues a path-based migration (dry-run or real) and
// returns the job id.
func (s *Manager) MigrateDriveByPathJob(ctx context.Context, prefix, driveID string, requireSize, dryRun bool) int {
	name := driveID
	for _, m := range s.driveSourceList() {
		if m.cfg.ID == driveID {
			name = m.cfg.Name
			break
		}
	}
	j := &driveMigrateJob{mgr: s, prefix: prefix, driveID: driveID, driveName: name, requireSize: requireSize, dryRun: dryRun}
	verb := "Migrate library to Drive"
	if dryRun {
		verb = "Preview Drive migration"
	}
	return s.JobManager.Add(ctx, fmt.Sprintf("%s: %s", verb, name), j)
}

// MigrateStats summarises a path-based Drive migration.
type MigrateStats struct {
	Candidates   int // files in the drive index
	Migrated     int // existing DB files repointed to the drive
	NotInDB      int // on the drive but not yet in the DB (a scan would add these)
	SizeMismatch int // DB file at the path but a different size
	Collision    int // a file already exists at the drive path (drive already scanned)
	DryRun       bool
}

// MigrateToDriveByPath repoints existing DB files onto a Drive source by matching
// relative path (and, by default, size) against the drive index — no hashing and
// no scan. File ids, fingerprints and scene links are preserved; only the file's
// parent folder (hence path) changes. Files present on the drive but not in the
// DB are left for a normal scan to create.
func (s *Manager) MigrateToDriveByPath(ctx context.Context, prefix, driveID string, requireSize, dryRun bool, onProgress func(scanned, migrated, total int)) (MigrateStats, error) {
	stats := MigrateStats{DryRun: dryRun}

	var ms *managedDriveSource
	for _, m := range s.driveSourceList() {
		if m.cfg.ID == driveID {
			ms = m
			break
		}
	}
	if ms == nil {
		return stats, fmt.Errorf("no mounted drive source with id %q", driveID)
	}
	driveRoot := ms.root
	idx := ms.source.Index
	prefix = strings.TrimRight(prefix, "/")
	prefixSlash := prefix + "/"

	files, err := idx.AllFiles()
	if err != nil {
		return stats, fmt.Errorf("reading drive index: %w", err)
	}
	stats.Candidates = len(files)
	logger.Infof("drive-migrate[%s]: %d files in index; matching under %s (dry_run=%v)", driveID, len(files), prefix, dryRun)
	report := func(scanned int) {
		if onProgress != nil {
			onProgress(scanned, stats.Migrated, len(files))
		}
	}
	report(0)

	repo := s.Repository
	driveRoots := s.DriveRoots()
	folderCache := map[string]models.FolderID{}

	// Preload (one paginated scan each) the DB file paths+sizes under the prefix,
	// and the paths already under the drive root, so matching is in-memory rather
	// than a point query per index file — far fewer queries / much less lock
	// contention than 100k+ individual FindByPath calls.
	var dbPaths map[string]int64
	driveExisting := make(map[string]struct{})
	if err := repo.WithReadTxn(ctx, func(ctx context.Context) error {
		var e error
		dbPaths, e = repo.File.FindPathSizes(ctx, []string{prefix})
		if e != nil {
			return e
		}
		drive, e := repo.File.FindPathSizes(ctx, []string{driveRoot})
		if e != nil {
			return e
		}
		for p := range drive {
			driveExisting[p] = struct{}{}
		}
		return nil
	}); err != nil {
		return stats, fmt.Errorf("preloading DB paths: %w", err)
	}
	logger.Infof("drive-migrate[%s]: preloaded %d DB paths under %s (%d already on drive)", driveID, len(dbPaths), prefix, len(driveExisting))

	processOne := func(ctx context.Context, rel string, size int64) error {
		mountPath := prefixSlash + rel
		dbSize, ok := dbPaths[mountPath]
		if !ok {
			stats.NotInDB++
			return nil
		}
		if requireSize && dbSize != size {
			stats.SizeMismatch++
			return nil
		}
		if dryRun {
			stats.Migrated++
			return nil
		}

		drivePath := driveRoot + "/" + rel
		if _, clash := driveExisting[drivePath]; clash {
			stats.Collision++
			return nil
		}
		driveDir := path.Dir(drivePath)
		folderID, ok := folderCache[driveDir]
		if !ok {
			fldr, err := file.GetOrCreateFolderHierarchy(ctx, repo.Folder, driveDir, driveRoots)
			if err != nil {
				return fmt.Errorf("folder %q: %w", driveDir, err)
			}
			folderID = fldr.ID
			folderCache[driveDir] = folderID
		}
		dbf, err := repo.File.FindByPath(ctx, mountPath, true)
		if err != nil {
			return err
		}
		if dbf == nil {
			stats.NotInDB++
			return nil
		}
		b := dbf.Base()
		b.ParentFolderID = folderID
		b.Path = drivePath
		if err := repo.File.Update(ctx, dbf); err != nil {
			return fmt.Errorf("update file %d: %w", b.ID, err)
		}
		driveExisting[drivePath] = struct{}{} // guard re-collision within this run
		stats.Migrated++
		return nil
	}

	if dryRun {
		// pure in-memory map lookups — no per-file DB query
		for i, f := range files {
			_ = processOne(ctx, f.RelPath, f.Size)
			if i%2000 == 0 {
				report(i)
			}
			if ctx.Err() != nil {
				return stats, ctx.Err()
			}
		}
		report(len(files))
		logger.Infof("drive-migrate[%s]: DRY RUN would migrate %d (not-in-db %d, size-mismatch %d)", driveID, stats.Migrated, stats.NotInDB, stats.SizeMismatch)
		return stats, nil
	}

	const batch = 500
	for i := 0; i < len(files); i += batch {
		end := i + batch
		if end > len(files) {
			end = len(files)
		}
		chunk := files[i:end]
		if err := repo.WithTxn(ctx, func(ctx context.Context) error {
			for _, f := range chunk {
				if err := processOne(ctx, f.RelPath, f.Size); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return stats, err
		}
		report(end)
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
	}

	logger.Infof("drive-migrate[%s]: migrated %d, not-in-db %d, size-mismatch %d, collision %d",
		driveID, stats.Migrated, stats.NotInDB, stats.SizeMismatch, stats.Collision)
	return stats, nil
}

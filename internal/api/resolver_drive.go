package api

import (
	"context"
	"time"

	"github.com/stashapp/stash/internal/manager"
)

func driveSourceToGQL(st manager.DriveSourceStatus) *DriveSource {
	ds := &DriveSource{
		ID:        st.ID,
		Name:      st.Name,
		DriveID:   st.DriveID,
		KeysPath:  st.KeysPath,
		FileCount: st.FileCount,
		Mounted:   st.Mounted,
		Path:      st.Path,
	}
	if st.RootFolderID != "" {
		r := st.RootFolderID
		ds.RootFolderID = &r
	}
	if st.Scope != "" {
		s := st.Scope
		ds.Scope = &s
	}
	if st.CacheDir != "" {
		d := st.CacheDir
		ds.CacheDir = &d
	}
	return ds
}

func (r *queryResolver) DriveSources(ctx context.Context) ([]*DriveSource, error) {
	statuses := manager.GetInstance().ListDriveSources()
	out := make([]*DriveSource, 0, len(statuses))
	for _, st := range statuses {
		out = append(out, driveSourceToGQL(st))
	}
	return out, nil
}

func (r *queryResolver) RcloneRemotes(ctx context.Context) ([]string, error) {
	return manager.GetInstance().RcloneRemotes()
}

func (r *queryResolver) GoogleAuthStatus(ctx context.Context) (*GoogleAuthStatus, error) {
	st := manager.GetInstance().GoogleAuthStatus("")
	return &GoogleAuthStatus{Connected: st.Connected, ClientConfigured: st.ClientConfigured}, nil
}

func (r *queryResolver) GoogleDrives(ctx context.Context) ([]*GoogleDrive, error) {
	drives, err := manager.GetInstance().ListGoogleDrives(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*GoogleDrive, 0, len(drives))
	for _, d := range drives {
		out = append(out, &GoogleDrive{ID: d.ID, Name: d.Name, MyDrive: d.MyDrive})
	}
	return out, nil
}

func (r *mutationResolver) SetGoogleOAuthClient(ctx context.Context, input SetGoogleOAuthClientInput) (bool, error) {
	if err := manager.GetInstance().SetGoogleOAuthClient(input.ClientID, input.ClientSecret); err != nil {
		return false, err
	}
	return true, nil
}

func (r *mutationResolver) DisconnectGoogleDrive(ctx context.Context, clearClient *bool) (bool, error) {
	clear := false
	if clearClient != nil {
		clear = *clearClient
	}
	if err := manager.GetInstance().DisconnectGoogle(clear); err != nil {
		return false, err
	}
	return true, nil
}

func (r *mutationResolver) MigrateDriveByPath(ctx context.Context, input MigrateDriveByPathInput) (*MigrateDriveResult, error) {
	requireSize := true
	if input.RequireSize != nil {
		requireSize = *input.RequireSize
	}
	dryRun := false
	if input.DryRun != nil {
		dryRun = *input.DryRun
	}
	// Both dry-run (preview) and real run execute as background jobs so they show
	// on the Tasks page, survive navigation, and don't time out the request.
	// Results are read back via driveMigrateStatus.
	manager.GetInstance().MigrateDriveByPathJob(ctx, input.Prefix, input.DriveID, requireSize, dryRun)
	return &MigrateDriveResult{DryRun: dryRun}, nil
}

func (r *queryResolver) DriveMigrateStatus(ctx context.Context) (*MigrateDriveStatus, error) {
	st := manager.GetInstance().GetMigrateStatus()
	out := &MigrateDriveStatus{
		Running:      st.Running,
		DryRun:       st.DryRun,
		Candidates:   st.Stats.Candidates,
		Migrated:     st.Stats.Migrated,
		NotInDb:      st.Stats.NotInDB,
		SizeMismatch: st.Stats.SizeMismatch,
		Collision:    st.Stats.Collision,
	}
	if st.DriveID != "" {
		d := st.DriveID
		out.DriveID = &d
	}
	if st.FinishedAt != nil {
		t := st.FinishedAt.Format(time.RFC3339)
		out.FinishedAt = &t
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (r *queryResolver) DriveBrowse(ctx context.Context, input DriveBrowseInput) ([]*DriveFolder, error) {
	params := manager.DriveSourceParams{
		DriveID:      deref(input.DriveID),
		KeysPath:     deref(input.KeysPath),
		Scope:        deref(input.Scope),
		AuthType:     deref(input.AuthType),
		ClientID:     deref(input.ClientID),
		ClientSecret: deref(input.ClientSecret),
		Token:        deref(input.Token),
		RcloneRemote: deref(input.RcloneRemote),
	}
	folders, err := manager.GetInstance().BrowseDrive(ctx, params, deref(input.ParentID))
	if err != nil {
		return nil, err
	}
	out := make([]*DriveFolder, 0, len(folders))
	for _, f := range folders {
		out = append(out, &DriveFolder{ID: f.ID, Name: f.Name})
	}
	return out, nil
}

func (r *mutationResolver) AddDriveSource(ctx context.Context, input AddDriveSourceInput) (*DriveSource, error) {
	params := manager.DriveSourceParams{
		ID:           input.ID,
		Name:         input.Name,
		DriveID:      deref(input.DriveID),
		RootFolderID: deref(input.RootFolderID),
		KeysPath:     deref(input.KeysPath),
		Scope:        deref(input.Scope),
		CacheDir:     deref(input.CacheDir),
		AuthType:     deref(input.AuthType),
		ClientID:     deref(input.ClientID),
		ClientSecret: deref(input.ClientSecret),
		Token:        deref(input.Token),
		RcloneRemote: deref(input.RcloneRemote),
	}
	if input.CacheBytes != nil {
		params.CacheBytes = *input.CacheBytes
	}
	if input.FastScan != nil {
		params.FastScan = *input.FastScan
	}

	if err := manager.GetInstance().AddDriveSource(ctx, params); err != nil {
		return nil, err
	}

	// return the freshly-mounted source's status
	for _, st := range manager.GetInstance().ListDriveSources() {
		if st.ID == params.ID {
			return driveSourceToGQL(st), nil
		}
	}
	return driveSourceToGQL(manager.DriveSourceStatus{
		ID: params.ID, Name: params.Name, DriveID: params.DriveID, KeysPath: params.KeysPath,
	}), nil
}

func (r *mutationResolver) RemoveDriveSource(ctx context.Context, id string) (bool, error) {
	if err := manager.GetInstance().RemoveDriveSource(ctx, id); err != nil {
		return false, err
	}
	return true, nil
}

func (r *mutationResolver) SyncDriveSource(ctx context.Context, id string) (bool, error) {
	if _, err := manager.GetInstance().SyncDriveSourceByID(ctx, id); err != nil {
		return false, err
	}
	return true, nil
}

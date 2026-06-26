package api

import (
	"context"

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
	if err := manager.GetInstance().SyncDriveSourceByID(id); err != nil {
		return false, err
	}
	return true, nil
}

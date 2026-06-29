package video

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stashapp/stash/pkg/ffmpeg"
	"github.com/stashapp/stash/pkg/mediapath"
	"github.com/stashapp/stash/pkg/models"
)

// fastContainer maps a file extension to the ffprobe container_name stash
// expects, for the fast-scan path (no ffprobe).
func fastContainer(p string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(p), ".")) {
	case "mkv", "webm":
		return "matroska,webm"
	case "mp4", "m4v", "mov":
		return "mov,mp4,m4a,3gp,3g2,mj2"
	case "ts", "m2ts", "mts":
		return "mpegts"
	case "avi":
		return "avi"
	case "wmv", "asf":
		return "asf"
	case "flv":
		return "flv"
	case "mpg", "mpeg":
		return "mpeg"
	case "ext":
		return ""
	default:
		return strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))
	}
}

// Decorator adds video specific fields to a File.
type Decorator struct {
	FFProbe *ffmpeg.FFProbe
}

func (d *Decorator) Decorate(ctx context.Context, fs models.FS, f models.File) (models.File, error) {
	if d.FFProbe == nil {
		return f, errors.New("ffprobe not configured")
	}

	base := f.Base()
	// ffprobe cannot read files inside zip archives directly.
	if base.ZipFile != nil {
		return f, fmt.Errorf("video.constructFile: zip-contained files are not supported")
	}

	// Fast scan: a backend (Google Drive) can supply duration/dimensions without
	// reading the file. When enabled for the source, skip ffprobe entirely. Codec
	// details are left blank (the player transcodes; a later full scan can fill
	// them) but the file is otherwise complete so it isn't re-probed every scan.
	if meta, ok, _ := mediapath.FastMeta(base.Path); ok {
		interactive := false
		if _, err := fs.Lstat(GetFunscriptPath(base.Path)); err == nil {
			interactive = true
		}
		return &models.VideoFile{
			BaseFile:    base,
			Format:      fastContainer(base.Path),
			VideoCodec:  "",
			AudioCodec:  "",
			Width:       int(meta.Width),
			Height:      int(meta.Height),
			Duration:    float64(meta.DurationMS) / 1000.0,
			FrameRate:   0,
			BitRate:     0,
			Interactive: interactive,
		}, nil
	}

	probe := d.FFProbe
	// Drive-backed paths are probed via an authenticated ranged URL (only the
	// container metadata is fetched, not the whole file); local paths are
	// probed directly by path.
	var videoFile *ffmpeg.VideoFile
	var err error
	if url, headers, ok, perr := mediapath.ProbeTarget(base.Path); perr != nil {
		return f, fmt.Errorf("resolving probe target for %q: %w", base.Path, perr)
	} else if ok {
		videoFile, err = probe.NewVideoFileWithHeaders(url, headers, base.Path)
	} else {
		videoFile, err = probe.NewVideoFile(base.Path)
	}
	if err != nil {
		return f, fmt.Errorf("running ffprobe on %q: %w", base.Path, err)
	}

	container, err := ffmpeg.MatchContainer(videoFile.Container, base.Path)
	if err != nil {
		return f, fmt.Errorf("matching container for %q: %w", base.Path, err)
	}

	// check if there is a funscript file
	interactive := false
	if _, err := fs.Lstat(GetFunscriptPath(base.Path)); err == nil {
		interactive = true
	}

	return &models.VideoFile{
		BaseFile:    base,
		Format:      string(container),
		VideoCodec:  videoFile.VideoCodec,
		AudioCodec:  videoFile.AudioCodec,
		Width:       videoFile.Width,
		Height:      videoFile.Height,
		Duration:    videoFile.FileDuration,
		FrameRate:   videoFile.FrameRate,
		BitRate:     videoFile.Bitrate,
		Interactive: interactive,
	}, nil
}

func (d *Decorator) IsMissingMetadata(ctx context.Context, fs models.FS, f models.File) bool {
	const (
		unsetString = "unset"
		unsetNumber = -1
	)

	vf, ok := f.(*models.VideoFile)
	if !ok {
		return true
	}

	interactive := false
	if _, err := fs.Lstat(GetFunscriptPath(vf.Base().Path)); err == nil {
		interactive = true
	}

	return vf.VideoCodec == unsetString || vf.AudioCodec == unsetString ||
		vf.Format == unsetString || vf.Width == unsetNumber ||
		vf.Height == unsetNumber || vf.FrameRate == unsetNumber ||
		vf.Duration == unsetNumber ||
		vf.BitRate == unsetNumber || interactive != vf.Interactive
}

package manager

import (
	"fmt"

	"golang.org/x/sync/singleflight"

	"github.com/stashapp/stash/pkg/fsutil"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/mediapath"
)

// spriteSF dedupes concurrent on-demand sprite generation per scene hash so two
// scrub requests don't both generate (and corrupt the non-atomic JPEG write).
var spriteSF singleflight.Group

func bothExist(a, b string) bool {
	ea, _ := fsutil.FileExists(a)
	eb, _ := fsutil.FileExists(b)
	return ea && eb
}

// EnsureSprite lazily generates the scrub sprite image + thumbs VTT for a scene
// if they're missing, resolving a Drive-backed path to a local cached file
// first. Synchronous (the caller is a serve handler) and deduped per hash.
func (s *Manager) EnsureSprite(scenePath, sceneHash string) error {
	if sceneHash == "" {
		return fmt.Errorf("no scene hash")
	}
	imagePath := s.Paths.Scene.GetSpriteImageFilePath(sceneHash)
	vttPath := s.Paths.Scene.GetSpriteVttFilePath(sceneHash)
	if bothExist(imagePath, vttPath) {
		return nil
	}
	if scenePath == "" {
		return fmt.Errorf("no scene path")
	}

	_, err, _ := spriteSF.Do(sceneHash, func() (interface{}, error) {
		if bothExist(imagePath, vttPath) {
			return nil, nil
		}
		// Resolve Drive paths to a local cached file (no-op for local paths).
		local, err := mediapath.Resolve(scenePath)
		if err != nil {
			return nil, fmt.Errorf("resolving %q: %w", scenePath, err)
		}
		videoFile, err := s.FFProbe.NewVideoFile(local)
		if err != nil {
			return nil, fmt.Errorf("probing %q: %w", local, err)
		}

		cfg := DefaultSpriteGeneratorConfig
		cfg.SpriteSize = s.Config.GetSpriteScreenshotSize()
		if s.Config.GetUseCustomSpriteInterval() {
			cfg.MinimumSprites = s.Config.GetMinimumSprites()
			cfg.MaximumSprites = s.Config.GetMaximumSprites()
			cfg.SpriteInterval = s.Config.GetSpriteInterval()
		}

		gen, err := NewSpriteGenerator(*videoFile, sceneHash, imagePath, vttPath, cfg)
		if err != nil {
			return nil, err
		}
		logger.Infof("on-demand sprite generation for scene %s", sceneHash)
		if err := gen.Generate(); err != nil {
			return nil, err
		}
		return nil, nil
	})
	return err
}

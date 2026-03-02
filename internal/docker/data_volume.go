package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
)

// VolumeName generates a unique volume name for a challenge based on its ID and ref
// The hash ensures cache invalidation when the challenge ref changes
func VolumeName(challengeID, ref string) string {
	hash := sha256.Sum256([]byte(ref))
	shortHash := hex.EncodeToString(hash[:])[:12]
	return fmt.Sprintf("dragrace-%s-%s", challengeID, shortHash)
}

// EnsureDataDir creates a Docker volume if it doesn't exist
func (e *Executor) EnsureDataDir(ctx context.Context, name string) error {
	// Check if volume already exists
	if e.DataDirExists(ctx, name) {
		return nil
	}

	// Create the volume
	_, err := e.client.VolumeCreate(ctx, volume.CreateOptions{
		Name: name,
		Labels: map[string]string{
			"created-by": "dragrace-runner",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create volume %s: %w", name, err)
	}

	return nil
}

// DataDirExists checks if a Docker volume exists
func (e *Executor) DataDirExists(ctx context.Context, name string) bool {
	args := filters.NewArgs()
	args.Add("name", name)

	volumes, err := e.client.VolumeList(ctx, volume.ListOptions{Filters: args})
	if err != nil {
		return false
	}

	for _, v := range volumes.Volumes {
		if v.Name == name {
			return true
		}
	}

	return false
}

// RemoveDataDir removes a Docker volume
func (e *Executor) RemoveDataDir(ctx context.Context, name string) error {
	return e.client.VolumeRemove(ctx, name, true)
}

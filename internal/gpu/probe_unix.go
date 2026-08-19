//go:build !windows

package gpu

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// statDeviceGroup returns the group id owning a host device node. The
// container has to join that group to open the node, since job containers
// run as a fixed non-root user.
func statDeviceGroup(path string) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		// The path is already reported by the caller; keep only the reason.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return 0, pathErr.Err
		}
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot read the owning group of %s on this platform", path)
	}
	return int(stat.Gid), nil
}

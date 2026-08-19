//go:build windows

package gpu

import "errors"

// statDeviceGroup has no meaning on Windows: there are no device nodes to
// map, and neither GPU passthrough mechanism this package supports exists
// there. Only RUNNER_GPUS=none is usable on a Windows host.
func statDeviceGroup(string) (int, error) {
	return 0, errors.New("host device mapping is not supported on Windows")
}

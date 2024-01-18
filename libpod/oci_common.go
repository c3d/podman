//go:build !remote && (linux || freebsd)

package libpod

import (
	"fmt"
	"strings"
	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v4/libpod/define"
)

// Make a new OCI runtime with the given options.
// Select between a Conmon-based runtime or a shimv2 runtime based on the name
// Deliberately private. Someone should not be able to construct this outside of
// libpod.
func newOCIRuntime(name string, paths []string, conmonPath string, runtimeFlags []string, runtimeCfg *config.Config) (OCIRuntime, error) {
	if name == "" {
		return nil, fmt.Errorf("the OCI runtime must be provided a non-empty name: %w", define.ErrInvalidArg)
	}
	if strings.Contains(name, "v2") {
		return newShimV2OCIRuntime(name, paths, runtimeFlags, runtimeCfg)
	}
	return newConmonOCIRuntime(name, paths, conmonPath, runtimeFlags, runtimeCfg)
}

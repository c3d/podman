//go:build !remote

package libpod

import (
	"errors"
	"os"
	"os/exec"
)

func (r *ShimV2OCIRuntime) createRootlessContainer(ctr *Container, restoreOptions *ContainerCheckpointOptions) (int64, error) {
	return -1, errors.New("unsupported (*ShimV2OCIRuntime) createRootlessContainer")
}

// Run the closure with the container's socket label set
func (r *ShimV2OCIRuntime) withContainerSocketLabel(ctr *Container, closure func() error) error {
	// No label support yet
	return closure()
}

// moveShimV2ToCgroupAndSignal gets a container's cgroupParent and moves the shimV2 process to that cgroup
// it then signals for shimV2 to start by sending nonce data down the start fd
func (r *ShimV2OCIRuntime) moveShimV2ToCgroupAndSignal(ctr *Container, cmd *exec.Cmd, startFd *os.File) error {
	// No equivalent to cgroup on FreeBSD, just signal shimV2 to start
	if err := writeShimPipeData(startFd); err != nil {
		return err
	}
	return nil
}

func moveToRuntimeCgroup() error {
	return errors.New("moveToRuntimeCgroup not supported on freebsd")
}


func newTTRPCServer() (*ttrpc.Server, error) {
	return ttrpc.NewServer()
}

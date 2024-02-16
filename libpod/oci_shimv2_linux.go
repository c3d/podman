//go:build !remote

package libpod

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containers/common/pkg/cgroups"
	"github.com/containers/common/pkg/config"
	"github.com/containers/common/pkg/systemd"
	"github.com/containers/podman/v5/pkg/errorhandling"
	"github.com/containers/podman/v5/pkg/rootless"
	pmount "github.com/containers/storage/pkg/mount"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	// DefaultAddressDir is the default location for the GRPC socket address
	DefaultAddressDir = "/run/containers"
)

func (r *ShimV2OCIRuntime) createRootlessContainer(ctr *Container, restoreOptions *ContainerCheckpointOptions) (int64, error) {
	type result struct {
		restoreDuration int64
		err             error
	}
	ch := make(chan result)
	go func() {
		runtime.LockOSThread()
		restoreDuration, err := func() (int64, error) {
			fd, err := os.Open(fmt.Sprintf("/proc/%d/task/%d/ns/mnt", os.Getpid(), unix.Gettid()))
			if err != nil {
				return 0, err
			}
			defer errorhandling.CloseQuiet(fd)

			// create a new mountns on the current thread
			if err = unix.Unshare(unix.CLONE_NEWNS); err != nil {
				return 0, err
			}
			defer func() {
				if err := unix.Setns(int(fd.Fd()), unix.CLONE_NEWNS); err != nil {
					logrus.Errorf("Unable to clone new namespace: %q", err)
				}
			}()

			// don't spread our mounts around.  We are setting only /sys to be slave
			// so that the cleanup process is still able to umount the storage and the
			// changes are propagated to the host.
			err = unix.Mount("/sys", "/sys", "none", unix.MS_REC|unix.MS_SLAVE, "")
			if err != nil {
				return 0, fmt.Errorf("cannot make /sys slave: %w", err)
			}

			mounts, err := pmount.GetMounts()
			if err != nil {
				return 0, err
			}
			for _, m := range mounts {
				if !strings.HasPrefix(m.Mountpoint, "/sys/kernel") {
					continue
				}
				err = unix.Unmount(m.Mountpoint, 0)
				if err != nil && !os.IsNotExist(err) {
					return 0, fmt.Errorf("cannot unmount %s: %w", m.Mountpoint, err)
				}
			}
			return r.createShimV2Task(ctr, restoreOptions)
		}()
		ch <- result{
			restoreDuration: restoreDuration,
			err:             err,
		}
	}()
	res := <-ch
	return res.restoreDuration, res.err
}

// Run the closure with the container's socket label set
func (r *ShimV2OCIRuntime) withContainerSocketLabel(ctr *Container, closure func() error) error {
	runtime.LockOSThread()
	if err := label.SetSocketLabel(ctr.ProcessLabel()); err != nil {
		return err
	}
	err := closure()
	// Ignore error returned from SetSocketLabel("") call,
	// can't recover.
	if labelErr := label.SetSocketLabel(""); labelErr == nil {
		// Unlock the thread only if the process label could be restored
		// successfully.  Otherwise leave the thread locked and the Go runtime
		// will terminate it once it returns to the threads pool.
		runtime.UnlockOSThread()
	} else {
		logrus.Errorf("Unable to reset socket label: %q", labelErr)
	}
	return err
}

// moveShimV2ToCgroupAndSignal gets a container's cgroupParent and moves the shimV2 process to that cgroup
// it then signals for shimV2 to start by sending nonce data down the start fd
func (r *ShimV2OCIRuntime) moveShimV2ToCgroupAndSignal(ctr *Container, cmd *exec.Cmd, startFd *os.File) error {
	mustCreateCgroup := true

	if ctr.config.NoCgroups {
		mustCreateCgroup = false
	}

	// If cgroup creation is disabled - just signal.
	switch ctr.config.CgroupsMode {
	case "disabled", "no-shimV2", cgroupSplit:
		mustCreateCgroup = false
	}

	// $INVOCATION_ID is set by systemd when running as a service.
	if ctr.runtime.RemoteURI() == "" && os.Getenv("INVOCATION_ID") != "" {
		mustCreateCgroup = false
	}

	if mustCreateCgroup {
		// Usually rootless users are not allowed to configure cgroupfs.
		// There are cases though, where it is allowed, e.g. if the cgroup
		// is manually configured and chowned).  Avoid detecting all
		// such cases and simply use a lower log level.
		logLevel := logrus.WarnLevel
		if rootless.IsRootless() {
			logLevel = logrus.InfoLevel
		}
		// TODO: This should be a switch - we are not guaranteed that
		// there are only 2 valid cgroup managers
		cgroupParent := ctr.CgroupParent()
		cgroupPath := filepath.Join(ctr.config.CgroupParent, "shimV2")
		cgroupResources, err := GetLimits(ctr.LinuxResources())
		if err != nil {
			logrus.StandardLogger().Log(logLevel, "Could not get ctr resources")
		}
		if ctr.CgroupManager() == config.SystemdCgroupsManager {
			unitName := createUnitName("libpod-shimV2", ctr.ID())
			realCgroupParent := cgroupParent
			splitParent := strings.Split(cgroupParent, "/")
			if strings.HasSuffix(cgroupParent, ".slice") && len(splitParent) > 1 {
				realCgroupParent = splitParent[len(splitParent)-1]
			}

			logrus.Infof("Running shimV2 under slice %s and unitName %s", realCgroupParent, unitName)
			if err := systemd.RunUnderSystemdScope(cmd.Process.Pid, realCgroupParent, unitName); err != nil {
				logrus.StandardLogger().Logf(logLevel, "Failed to add shimV2 to systemd sandbox cgroup: %v", err)
			}
		} else {
			control, err := cgroups.New(cgroupPath, &cgroupResources)
			if err != nil {
				logrus.StandardLogger().Logf(logLevel, "Failed to add shimV2 to cgroupfs sandbox cgroup: %v", err)
			} else if err := control.AddPid(cmd.Process.Pid); err != nil {
				// we need to remove this defer and delete the cgroup once shimV2 exits
				// maybe need a shimV2 monitor?
				logrus.StandardLogger().Logf(logLevel, "Failed to add shimV2 to cgroupfs sandbox cgroup: %v", err)
			}
		}
	}

	/* We set the cgroup, now the child can start creating children */
	return writeShimPipeData(startFd)
}

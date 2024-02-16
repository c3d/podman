//go:build !remote && (linux || freebsd)

package libpod

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"text/template"

	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v5/libpod/define"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// Make a new OCI runtime with the given options.
// Select between a Conmon-based runtime or a shimv2 runtime based on the name
// Deliberately private. Someone should not be able to construct this outside of
// libpod.
func newOCIRuntime(name string, paths []string, conmonPath string, runtimeFlags []string, runtimeCfg *config.Config) (OCIRuntime, error) {
	if name == "" {
		return nil, fmt.Errorf("the OCI runtime must be provided a non-empty name: %w", define.ErrInvalidArg)
	}
	logrus.Debugf("New OCI runtime named '%s'", name)
	if isShimV2Runtime(name) {
		return newShimV2OCIRuntime(name, paths, runtimeFlags, runtimeCfg)
	}
	return newConmonOCIRuntime(name, paths, conmonPath, runtimeFlags, runtimeCfg)
}


// Check if we have a shim v2 runtime
func isShimV2Runtime(name string) bool {
	return strings.HasSuffix(name, ".v2") || strings.HasSuffix(name, "-v2") || strings.Contains(name, "containerd-shim") || strings.Contains(name, "containerd.shim")
}

// hasCurrentUserMapped checks whether the current user is mapped inside the container user namespace
func hasCurrentUserMapped(ctr *Container) bool {
	if len(ctr.config.IDMappings.UIDMap) == 0 && len(ctr.config.IDMappings.GIDMap) == 0 {
		return true
	}
	uid := os.Geteuid()
	for _, m := range ctr.config.IDMappings.UIDMap {
		if uid >= m.HostID && uid < m.HostID+m.Size {
			return true
		}
	}
	return false
}

func generateResourceFile(res *spec.LinuxResources) (string, []string, error) {
	flags := []string{}
	if res == nil {
		return "", flags, nil
	}

	f, err := os.CreateTemp("", "podman")
	if err != nil {
		return "", nil, err
	}

	j, err := json.Marshal(res)
	if err != nil {
		return "", nil, err
	}
	_, err = f.Write(j)
	if err != nil {
		return "", nil, err
	}

	flags = append(flags, "--resources="+f.Name())
	return f.Name(), flags, nil
}

// This filters out ENOTCONN errors which can happen on FreeBSD if the
// other side of the connection is already closed.
func socketCloseWrite(conn *net.UnixConn) error {
	err := conn.CloseWrite()
	if err != nil && errors.Is(err, syscall.ENOTCONN) {
		return nil
	}
	return err
}

// isRetryable returns whether the error was caused by a blocked syscall or the
// specified operation on a non blocking file descriptor wasn't ready for completion.
func isRetryable(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EINTR || errno == syscall.EAGAIN
	}
	return false
}

// openControlFile opens the terminal control file.
func openControlFile(ctr *Container, parentDir string) (*os.File, error) {
	controlPath := filepath.Join(parentDir, "ctl")
	for i := 0; i < 600; i++ {
		controlFile, err := os.OpenFile(controlPath, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			return controlFile, nil
		}
		if !isRetryable(err) {
			return nil, fmt.Errorf("could not open ctl file for terminal resize for container %s: %w", ctr.ID(), err)
		}
		time.Sleep(time.Second / 10)
	}
	return nil, fmt.Errorf("timeout waiting for %q", controlPath)
}

// Wait for a container which has been sent a signal to stop
func waitContainerStop(ctr *Container, timeout time.Duration) error {
	return waitPidStop(ctr.state.PID, timeout)
}

// Wait for a given PID to stop
func waitPidStop(pid int, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return fmt.Errorf("given PID did not die within timeout")
		default:
			if err := unix.Kill(pid, 0); err != nil {
				if err == unix.ESRCH {
					return nil
				}
				logrus.Errorf("Pinging PID %d with signal 0: %v", pid, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func getPreserveFdExtraFiles(preserveFD []uint, preserveFDs uint) (uint, []*os.File, []*os.File, error) {
	var filesToClose []*os.File
	var extraFiles []*os.File

	preserveFDsMap := make(map[uint]struct{})
	for _, i := range preserveFD {
		if i < 3 {
			return 0, nil, nil, fmt.Errorf("cannot preserve FD %d, consider using the passthrough log-driver to pass STDIO streams into the container: %w", i, define.ErrInvalidArg)
		}
		if i-2 > preserveFDs {
			// preserveFDs is the number of FDs above 2 to keep around.
			// e.g. if the user specified FD=3, then preserveFDs must be 1.
			preserveFDs = i - 2
		}
		preserveFDsMap[i] = struct{}{}
	}

	if preserveFDs > 0 {
		for fd := 3; fd < int(3+preserveFDs); fd++ {
			if len(preserveFDsMap) > 0 {
				if _, ok := preserveFDsMap[uint(fd)]; !ok {
					extraFiles = append(extraFiles, nil)
					continue
				}
			}
			f := os.NewFile(uintptr(fd), fmt.Sprintf("fd-%d", fd))
			filesToClose = append(filesToClose, f)
			extraFiles = append(extraFiles, f)
		}
	}
	return preserveFDs, filesToClose, extraFiles, nil
}

// newPipe creates a unix socket pair for communication.
// Returns two files - first is parent, second is child.
func newPipe() (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_LOCAL, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[1]), "parent"), os.NewFile(uintptr(fds[0]), "child"), nil
}

// formatRuntimeOpts prepends opts passed to it with --runtime-opt for passing to shimV2
func formatRuntimeOpts(opts ...string) []string {
	args := make([]string, 0, len(opts)*2)
	for _, o := range opts {
		args = append(args, "--runtime-opt", o)
	}
	return args
}


// makeAccessible changes the path permission and each parent directory to have --x--x--x
func makeAccessible(path string, uid, gid int) error {
	for ; path != "/"; path = filepath.Dir(path) {
		st, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if int(st.Sys().(*syscall.Stat_t).Uid) == uid && int(st.Sys().(*syscall.Stat_t).Gid) == gid {
			continue
		}
		if st.Mode()&0111 != 0111 {
			if err := os.Chmod(path, st.Mode()|0111); err != nil {
				return err
			}
		}
	}
	return nil
}

// Copy data from container to HTTP connection, for terminal attach.
// Container is the container's attach socket connection, http is a buffer for
// the HTTP connection. cid is the ID of the container the attach session is
// running for (used solely for error messages).
func httpAttachTerminalCopy(container *net.UnixConn, http *bufio.ReadWriter, cid string) error {
	buf := make([]byte, bufferSize)
	for {
		numR, err := container.Read(buf)
		logrus.Debugf("Read fd(%d) %d/%d bytes for container %s", int(buf[0]), numR, len(buf), cid)

		if numR > 0 {
			switch buf[0] {
			case AttachPipeStdout:
				// Do nothing
			default:
				logrus.Errorf("Received unexpected attach type %+d, discarding %d bytes", buf[0], numR)
				continue
			}

			numW, err2 := http.Write(buf[1:numR])
			if err2 != nil {
				if err != nil {
					logrus.Errorf("Reading container %s STDOUT: %v", cid, err)
				}
				return err2
			} else if numW+1 != numR {
				return io.ErrShortWrite
			}
			// We need to force the buffer to write immediately, so
			// there isn't a delay on the terminal side.
			if err2 := http.Flush(); err2 != nil {
				if err != nil {
					logrus.Errorf("Reading container %s STDOUT: %v", cid, err)
				}
				return err2
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// Copy data from a container to an HTTP connection, for non-terminal attach.
// Appends a header to multiplex input.
func httpAttachNonTerminalCopy(container *net.UnixConn, http *bufio.ReadWriter, cid string, stdin, stdout, stderr bool) error {
	buf := make([]byte, bufferSize)
	for {
		numR, err := container.Read(buf)
		if numR > 0 {
			var headerBuf []byte

			// Subtract 1 because we strip the first byte (used for
			// multiplexing by ShimV2).
			headerLen := uint32(numR - 1)
			// Practically speaking, we could make this buf[0] - 1,
			// but we need to validate it anyway.
			switch buf[0] {
			case AttachPipeStdin:
				headerBuf = makeHTTPAttachHeader(0, headerLen)
				if !stdin {
					continue
				}
			case AttachPipeStdout:
				if !stdout {
					continue
				}
				headerBuf = makeHTTPAttachHeader(1, headerLen)
			case AttachPipeStderr:
				if !stderr {
					continue
				}
				headerBuf = makeHTTPAttachHeader(2, headerLen)
			default:
				logrus.Errorf("Received unexpected attach type %+d, discarding %d bytes", buf[0], numR)
				continue
			}

			numH, err2 := http.Write(headerBuf)
			if err2 != nil {
				if err != nil {
					logrus.Errorf("Reading container %s standard streams: %v", cid, err)
				}

				return err2
			}
			// Hardcoding header length is pretty gross, but
			// fast. Should be safe, as this is a fixed part
			// of the protocol.
			if numH != 8 {
				if err != nil {
					logrus.Errorf("Reading container %s standard streams: %v", cid, err)
				}

				return io.ErrShortWrite
			}

			numW, err2 := http.Write(buf[1:numR])
			if err2 != nil {
				if err != nil {
					logrus.Errorf("Reading container %s standard streams: %v", cid, err)
				}

				return err2
			} else if numW+1 != numR {
				if err != nil {
					logrus.Errorf("Reading container %s standard streams: %v", cid, err)
				}

				return io.ErrShortWrite
			}
			// We need to force the buffer to write immediately, so
			// there isn't a delay on the terminal side.
			if err2 := http.Flush(); err2 != nil {
				if err != nil {
					logrus.Errorf("Reading container %s STDOUT: %v", cid, err)
				}
				return err2
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}

			return err
		}
	}
}


func getLogTag(ctr *Container) (string, error) {
	logTag := ctr.LogTag()
	if logTag == "" {
		return "", nil
	}
	data, err := ctr.inspectLocked(false)
	if err != nil {
		// FIXME: this error should probably be returned
		return "", nil //nolint: nilerr
	}
	tmpl, err := template.New("container").Parse(logTag)
	if err != nil {
		return "", fmt.Errorf("template parsing error %s: %w", logTag, err)
	}
	var b bytes.Buffer
	err = tmpl.Execute(&b, data)
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

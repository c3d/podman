//go:build !remote && (linux || freebsd)

package libpod

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/containers/common/pkg/config"
	"github.com/containers/common/pkg/detach"
	"github.com/containers/common/pkg/resize"
	"github.com/containers/common/pkg/version"
	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/libpod/logs"
	"github.com/containers/podman/v5/pkg/checkpoint/crutils"
	"github.com/containers/podman/v5/pkg/errorhandling"
	"github.com/containers/podman/v5/pkg/rootless"
	"github.com/containers/podman/v5/pkg/specgenutil"
	"github.com/containers/podman/v5/pkg/util"
	"github.com/containers/podman/v5/utils"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	//	shimV2 "github.com/containerd/containerd/runtime/v2"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// ShimV2OCIRuntime is an OCI runtime managed through the shimV2 interface,
// like Kata Containers.
// See https://github.com/containerd/containerd/blob/main/runtime/v2/README.md
type ShimV2OCIRuntime struct {
	name              string
	path              string
	socketPath        string
	shimV2Env         []string
	tmpDir            string
	exitsDir          string
	logSizeMax        int64
	noPivot           bool
	reservePorts      bool
	runtimeFlags      []string
	supportsJSON      bool
	supportsKVM       bool
	supportsNoCgroups bool
	enableKeyring     bool
}


// Make a new ShimV2-OCI runtime with the given options.
// The shimv2 interface will launche a _persistent_ process that acts as a shim,
// like the containerd-shim-kata-v2, which does not obey the runc-style
// command-line interface, but instead communicates over a socket.
// This requires podman to fork a process that stays alive in the background
// and acts as a containerd-compatible server for the ShimV2 RPC calls.
// The first path that points to a valid executable will be used.
// Deliberately private. Someone should not be able to construct this outside of
// libpod.
func newShimV2OCIRuntime(name string, paths []string, runtimeFlags []string, runtimeCfg *config.Config)  (OCIRuntime, error) {
	// Make lookup tables for runtime support
	supportsJSON := make(map[string]bool, len(runtimeCfg.Engine.RuntimeSupportsJSON.Get()))
	supportsNoCgroups := make(map[string]bool, len(runtimeCfg.Engine.RuntimeSupportsNoCgroups.Get()))
	supportsKVM := make(map[string]bool, len(runtimeCfg.Engine.RuntimeSupportsKVM.Get()))
	for _, r := range runtimeCfg.Engine.RuntimeSupportsJSON.Get() {
		supportsJSON[r] = true
	}
	for _, r := range runtimeCfg.Engine.RuntimeSupportsNoCgroups.Get() {
		supportsNoCgroups[r] = true
	}
	for _, r := range runtimeCfg.Engine.RuntimeSupportsKVM.Get() {
		supportsKVM[r] = true
	}

	// Turn foo.bar.stuff.kata.v2 into containerd-shim-kata-v2
	if strings.Contains(name, ".v2") {
		fields := strings.Split(name, ".")
		lastTwo := fields[len(fields)-2:]
		name = "containerd-shim-" + strings.Join(lastTwo, "-")
	}

	runtime := new(ShimV2OCIRuntime)
	runtime.name = name
	runtime.runtimeFlags = runtimeFlags

	runtime.shimV2Env = runtimeCfg.Engine.ShimV2EnvVars.Get()
	runtime.tmpDir = runtimeCfg.Engine.TmpDir
	runtime.logSizeMax = runtimeCfg.Containers.LogSizeMax
	runtime.noPivot = runtimeCfg.Engine.NoPivotRoot
	runtime.reservePorts = runtimeCfg.Engine.EnablePortReservation
	runtime.enableKeyring = runtimeCfg.Containers.EnableKeyring

	// TODO: probe OCI runtime for feature and enable automatically if
	// available.

	base := filepath.Base(name)
	runtime.supportsJSON = supportsJSON[base]
	runtime.supportsNoCgroups = supportsNoCgroups[base]
	runtime.supportsKVM = supportsKVM[base]

	foundPath := false
	for _, path := range paths {
		stat, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("cannot stat OCI shim-V2 runtime %s path: %w", name, err)
		}
		if !stat.Mode().IsRegular() {
			continue
		}
		foundPath = true
		logrus.Tracef("found runtime %q", path)
		runtime.path = path
		break
	}

	// Search the $PATH as last fallback
	if !foundPath {
		if foundRuntime, err := exec.LookPath(name); err == nil {
			foundPath = true
			runtime.path = foundRuntime
			logrus.Debugf("using runtime %q from $PATH: %q", name, foundRuntime)
		}
	}

	if !foundPath {
		return nil, fmt.Errorf("no valid executable found for OCI runtime %s: %w", name, define.ErrInvalidArg)
	}

	runtime.exitsDir = filepath.Join(runtime.tmpDir, "exits")

	// Create the exit files and attach sockets directories
	if err := os.MkdirAll(runtime.exitsDir, 0750); err != nil {
		// The directory is allowed to exist
		if !os.IsExist(err) {
			return nil, fmt.Errorf("creating OCI runtime exit files directory: %w", err)
		}
	}
	return runtime, nil
}

// Name returns the name of the ShimV2 runtime
func (r *ShimV2OCIRuntime) Name() string {
	return r.name
}

// Path returns the path of the ShimV2 runtime binary
func (r *ShimV2OCIRuntime) Path() string {
	return r.path
}

// CreateContainer creates a container.
func (r *ShimV2OCIRuntime) CreateContainer(ctr *Container, restoreOptions *ContainerCheckpointOptions) (int64, error) {
	// always make the run dir accessible to the current user so that the PID files can be read without
	// being in the rootless user namespace.
	if err := makeAccessible(ctr.state.RunDir, 0, 0); err != nil {
		return 0, err
	}
	if !hasCurrentUserMapped(ctr) {
		for _, i := range []string{ctr.state.RunDir, ctr.runtime.config.Engine.TmpDir, ctr.config.StaticDir, ctr.state.Mountpoint, ctr.runtime.config.Engine.VolumePath} {
			if err := makeAccessible(i, ctr.RootUID(), ctr.RootGID()); err != nil {
				return 0, err
			}
		}

		// if we are running a non privileged container, be sure to umount some kernel paths so they are not
		// bind mounted inside the container at all.
		if !ctr.config.Privileged && !rootless.IsRootless() {
			return r.createRootlessContainer(ctr, restoreOptions)
		}
	}
	return r.createOCIContainer(ctr, restoreOptions)
}

// UpdateContainerStatus retrieves the current status of the container from the
// runtime. It updates the container's state but does not save it.
// If useRuntime is false, we will not directly hit runc to see the container's
// status, but will instead only check for the existence of the shimV2 exit file
// and update state to stopped if it exists.
func (r *ShimV2OCIRuntime) UpdateContainerStatus(ctr *Container) error {
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}

	// Store old state so we know if we were already stopped
	oldState := ctr.state.State

	state := new(spec.State)

	cmd := exec.Command(r.path, "state", ctr.ID())
	cmd.Env = append(cmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir))

	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("getting stdout pipe: %w", err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("getting stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		out, err2 := io.ReadAll(errPipe)
		if err2 != nil {
			return fmt.Errorf("getting container %s state: %w", ctr.ID(), err)
		}
		if strings.Contains(string(out), "does not exist") || strings.Contains(string(out), "No such file") {
			if err := ctr.removeShimV2Files(); err != nil {
				logrus.Debugf("unable to remove shimV2 files for container %s", ctr.ID())
			}
			ctr.state.ExitCode = -1
			ctr.state.FinishedTime = time.Now()
			ctr.state.State = define.ContainerStateExited
			return ctr.runtime.state.AddContainerExitCode(ctr.ID(), ctr.state.ExitCode)
		}
		return fmt.Errorf("getting container %s state. stderr/out: %s: %w", ctr.ID(), out, err)
	}
	defer func() {
		_ = cmd.Wait()
	}()

	if err := errPipe.Close(); err != nil {
		return err
	}
	out, err := io.ReadAll(outPipe)
	if err != nil {
		return fmt.Errorf("reading stdout: %s: %w", ctr.ID(), err)
	}
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(state); err != nil {
		return fmt.Errorf("decoding container status for container %s: %w", ctr.ID(), err)
	}
	ctr.state.PID = state.Pid

	switch state.Status {
	case "created":
		ctr.state.State = define.ContainerStateCreated
	case "paused":
		ctr.state.State = define.ContainerStatePaused
	case "running":
		ctr.state.State = define.ContainerStateRunning
	case "stopped":
		ctr.state.State = define.ContainerStateStopped
	default:
		return fmt.Errorf("unrecognized status returned by runtime for container %s: %s: %w",
			ctr.ID(), state.Status, define.ErrInternal)
	}

	// Handle ContainerStateStopping - keep it unless the container
	// transitioned to no longer running.
	if oldState == define.ContainerStateStopping && (ctr.state.State == define.ContainerStatePaused || ctr.state.State == define.ContainerStateRunning) {
		ctr.state.State = define.ContainerStateStopping
	}

	return nil
}

// StartContainer starts the given container.
// Sets time the container was started, but does not save it.
func (r *ShimV2OCIRuntime) StartContainer(ctr *Container) error {
	// TODO: streams should probably *not* be our STDIN/OUT/ERR - redirect to buffers?
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, fmt.Sprintf("PATH=%s", path))
	}
	if err := utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, append(r.runtimeFlags, "start", ctr.ID())...); err != nil {
		return err
	}

	ctr.state.StartedTime = time.Now()

	return nil
}

// UpdateContainer updates the given container's cgroup configuration
func (r *ShimV2OCIRuntime) UpdateContainer(ctr *Container, resources *spec.LinuxResources) error {
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, fmt.Sprintf("PATH=%s", path))
	}
	args := r.runtimeFlags
	args = append(args, "update")
	tempFile, additionalArgs, err := generateResourceFile(resources)
	if err != nil {
		return err
	}
	defer os.Remove(tempFile)

	args = append(args, additionalArgs...)
	return utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, append(args, ctr.ID())...)
}

// KillContainer sends the given signal to the given container.
// If all is set, send to all PIDs in the container.
// All is only supported if the container created cgroups.
func (r *ShimV2OCIRuntime) KillContainer(ctr *Container, signal uint, all bool) error {
	if _, err := r.killContainer(ctr, signal, all, false); err != nil {
		return err
	}

	return nil
}

// If captureStderr is requested, OCI runtime STDERR will be captured as a
// *bytes.buffer and returned; otherwise, it is set to os.Stderr.
func (r *ShimV2OCIRuntime) killContainer(ctr *Container, signal uint, all, captureStderr bool) (*bytes.Buffer, error) {
	logrus.Debugf("Sending signal %d to container %s", signal, ctr.ID())
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return nil, err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	var args []string
	args = append(args, r.runtimeFlags...)
	if all {
		args = append(args, "kill", "--all", ctr.ID(), strconv.FormatUint(uint64(signal), 10))
	} else {
		args = append(args, "kill", ctr.ID(), strconv.FormatUint(uint64(signal), 10))
	}
	var (
		stderr       io.Writer = os.Stderr
		stderrBuffer *bytes.Buffer
	)
	if captureStderr {
		stderrBuffer = new(bytes.Buffer)
		stderr = stderrBuffer
	}
	if err := utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, stderr, env, r.path, args...); err != nil {
		// Update container state - there's a chance we failed because
		// the container exited in the meantime.
		if err2 := r.UpdateContainerStatus(ctr); err2 != nil {
			logrus.Infof("Error updating status for container %s: %v", ctr.ID(), err2)
		}
		if ctr.ensureState(define.ContainerStateStopped, define.ContainerStateExited) {
			return stderrBuffer, fmt.Errorf("%w: %s", define.ErrCtrStateInvalid, ctr.state.State)
		}
		return stderrBuffer, fmt.Errorf("sending signal to container %s: %w", ctr.ID(), err)
	}

	return stderrBuffer, nil
}

// StopContainer stops a container, first using its given stop signal (or
// SIGTERM if no signal was specified), then using SIGKILL.
// Timeout is given in seconds. If timeout is 0, the container will be
// immediately kill with SIGKILL.
// Does not set finished time for container, assumes you will run updateStatus
// after to pull the exit code.
func (r *ShimV2OCIRuntime) StopContainer(ctr *Container, timeout uint, all bool) error {
	logrus.Debugf("Stopping container %s (PID %d)", ctr.ID(), ctr.state.PID)

	// Ping the container to see if it's alive
	// If it's not, it's already stopped, return
	err := unix.Kill(ctr.state.PID, 0)
	if err == unix.ESRCH {
		return nil
	}

	killCtr := func(signal uint) (bool, error) {
		stderr, err := r.killContainer(ctr, signal, all, true)
		if err != nil {
			// There's an inherent race with the cleanup process (see
			// #16142, #17142). If the container has already been marked as
			// stopped or exited by the cleanup process, we can return
			// immediately.
			if errors.Is(err, define.ErrCtrStateInvalid) && ctr.ensureState(define.ContainerStateStopped, define.ContainerStateExited) {
				return true, nil
			}

			// If the PID is 0, then the container is already stopped.
			if ctr.state.PID == 0 {
				return true, nil
			}

			// Is the container gone?
			// If so, it probably died between the first check and
			// our sending the signal
			// The container is stopped, so exit cleanly
			err := unix.Kill(ctr.state.PID, 0)
			if err == unix.ESRCH {
				return true, nil
			}

			return false, err
		}

		// Before handling error from KillContainer, convert STDERR to a []string
		// (one string per line of output) and print it.
		stderrLines := strings.Split(stderr.String(), "\n")
		for _, line := range stderrLines {
			if line != "" {
				fmt.Fprintf(os.Stderr, "%s\n", line)
			}
		}

		return false, nil
	}

	if timeout > 0 {
		stopSignal := ctr.config.StopSignal
		if stopSignal == 0 {
			stopSignal = uint(syscall.SIGTERM)
		}

		stopped, err := killCtr(stopSignal)
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}

		if err := waitContainerStop(ctr, time.Duration(util.ConvertTimeout(int(timeout)))*time.Second); err != nil {
			sigName := unix.SignalName(syscall.Signal(stopSignal))
			if sigName == "" {
				sigName = fmt.Sprintf("(%d)", stopSignal)
			}
			logrus.Debugf("Timed out stopping container %s with %s, resorting to SIGKILL: %v", ctr.ID(), sigName, err)
			logrus.Warnf("StopSignal %s failed to stop container %s in %d seconds, resorting to SIGKILL", sigName, ctr.Name(), timeout)
		} else {
			// No error, the container is dead
			return nil
		}
	}

	stopped, err := killCtr(uint(unix.SIGKILL))
	if err != nil {
		return fmt.Errorf("sending SIGKILL to container %s: %w", ctr.ID(), err)
	}
	if stopped {
		return nil
	}

	// Give runtime a few seconds to make it happen
	if err := waitContainerStop(ctr, killContainerTimeout); err != nil {
		return err
	}

	return nil
}

// DeleteContainer deletes a container from the OCI runtime.
func (r *ShimV2OCIRuntime) DeleteContainer(ctr *Container) error {
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	return utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, append(r.runtimeFlags, "delete", "--force", ctr.ID())...)
}

// PauseContainer pauses the given container.
func (r *ShimV2OCIRuntime) PauseContainer(ctr *Container) error {
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	return utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, append(r.runtimeFlags, "pause", ctr.ID())...)
}

// UnpauseContainer unpauses the given container.
func (r *ShimV2OCIRuntime) UnpauseContainer(ctr *Container) error {
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	return utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, append(r.runtimeFlags, "resume", ctr.ID())...)
}

// HTTPAttach performs an attach for the HTTP API.
// The caller must handle closing the HTTP connection after this returns.
// The cancel channel is not closed; it is up to the caller to do so after
// this function returns.
// If this is a container with a terminal, we will stream raw. If it is not, we
// will stream with an 8-byte header to multiplex STDOUT and STDERR.
// Returns any errors that occurred, and whether the connection was successfully
// hijacked before that error occurred.
func (r *ShimV2OCIRuntime) HTTPAttach(ctr *Container, req *http.Request, w http.ResponseWriter, streams *HTTPAttachStreams, detachKeys *string, cancel <-chan bool, hijackDone chan<- bool, streamAttach, streamLogs bool) (deferredErr error) {
	isTerminal := ctr.Terminal()

	if streams != nil {
		if !streams.Stdin && !streams.Stdout && !streams.Stderr {
			return fmt.Errorf("must specify at least one stream to attach to: %w", define.ErrInvalidArg)
		}
	}

	attachSock, err := r.AttachSocketPath(ctr)
	if err != nil {
		return err
	}

	var conn *net.UnixConn
	if streamAttach {
		newConn, err := openUnixSocket(attachSock)
		if err != nil {
			return fmt.Errorf("failed to connect to container's attach socket: %v: %w", attachSock, err)
		}
		conn = newConn
		defer func() {
			if err := conn.Close(); err != nil {
				logrus.Errorf("Unable to close container %s attach socket: %q", ctr.ID(), err)
			}
		}()

		logrus.Debugf("Successfully connected to container %s attach socket %s", ctr.ID(), attachSock)
	}

	detachString := ctr.runtime.config.Engine.DetachKeys
	if detachKeys != nil {
		detachString = *detachKeys
	}
	isDetach, err := processDetachKeys(detachString)
	if err != nil {
		return err
	}

	attachStdout := true
	attachStderr := true
	attachStdin := true
	if streams != nil {
		attachStdout = streams.Stdout
		attachStderr = streams.Stderr
		attachStdin = streams.Stdin
	}

	logrus.Debugf("Going to hijack container %s attach connection", ctr.ID())

	// Alright, let's hijack.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return fmt.Errorf("unable to hijack connection")
	}

	httpCon, httpBuf, err := hijacker.Hijack()
	if err != nil {
		return fmt.Errorf("hijacking connection: %w", err)
	}

	hijackDone <- true

	writeHijackHeader(req, httpBuf, isTerminal)

	// Force a flush after the header is written.
	if err := httpBuf.Flush(); err != nil {
		return fmt.Errorf("flushing HTTP hijack header: %w", err)
	}

	defer func() {
		hijackWriteErrorAndClose(deferredErr, ctr.ID(), isTerminal, httpCon, httpBuf)
	}()

	logrus.Debugf("Hijack for container %s attach session done, ready to stream", ctr.ID())

	// TODO: This is gross. Really, really gross.
	// I want to say we should read all the logs into an array before
	// calling this, in container_api.go, but that could take a lot of
	// memory...
	// On the whole, we need to figure out a better way of doing this,
	// though.
	logSize := 0
	if streamLogs {
		logrus.Debugf("Will stream logs for container %s attach session", ctr.ID())

		// Get all logs for the container
		logChan := make(chan *logs.LogLine)
		logOpts := new(logs.LogOptions)
		logOpts.Tail = -1
		logOpts.WaitGroup = new(sync.WaitGroup)
		errChan := make(chan error)
		go func() {
			var err error
			// In non-terminal mode we need to prepend with the
			// stream header.
			logrus.Debugf("Writing logs for container %s to HTTP attach", ctr.ID())
			for logLine := range logChan {
				if !isTerminal {
					device := logLine.Device
					var header []byte
					headerLen := uint32(len(logLine.Msg))
					if !logLine.Partial() {
						// we append an extra newline in this case so we need to increment the len as well
						headerLen++
					}
					logSize += len(logLine.Msg)
					switch strings.ToLower(device) {
					case "stdin":
						header = makeHTTPAttachHeader(0, headerLen)
					case "stdout":
						header = makeHTTPAttachHeader(1, headerLen)
					case "stderr":
						header = makeHTTPAttachHeader(2, headerLen)
					default:
						logrus.Errorf("Unknown device for log line: %s", device)
						header = makeHTTPAttachHeader(1, headerLen)
					}
					_, err = httpBuf.Write(header)
					if err != nil {
						break
					}
				}
				_, err = httpBuf.Write([]byte(logLine.Msg))
				if err != nil {
					break
				}
				if !logLine.Partial() {
					_, err = httpBuf.Write([]byte("\n"))
					if err != nil {
						break
					}
				}
				err = httpBuf.Flush()
				if err != nil {
					break
				}
			}
			errChan <- err
		}()
		if err := ctr.ReadLog(context.Background(), logOpts, logChan, 0); err != nil {
			return err
		}
		go func() {
			logOpts.WaitGroup.Wait()
			close(logChan)
		}()
		logrus.Debugf("Done reading logs for container %s, %d bytes", ctr.ID(), logSize)
		if err := <-errChan; err != nil {
			return err
		}
	}
	if !streamAttach {
		logrus.Debugf("Done streaming logs for container %s attach, exiting as attach streaming not requested", ctr.ID())
		return nil
	}

	logrus.Debugf("Forwarding attach output for container %s", ctr.ID())

	stdoutChan := make(chan error)
	stdinChan := make(chan error)

	// Handle STDOUT/STDERR
	go func() {
		var err error
		if isTerminal {
			// Hack: return immediately if attachStdout not set to
			// emulate Docker.
			// Basically, when terminal is set, STDERR goes nowhere.
			// Everything does over STDOUT.
			// Therefore, if not attaching STDOUT - we'll never copy
			// anything from here.
			logrus.Debugf("Performing terminal HTTP attach for container %s", ctr.ID())
			if attachStdout {
				err = httpAttachTerminalCopy(conn, httpBuf, ctr.ID())
			}
		} else {
			logrus.Debugf("Performing non-terminal HTTP attach for container %s", ctr.ID())
			err = httpAttachNonTerminalCopy(conn, httpBuf, ctr.ID(), attachStdin, attachStdout, attachStderr)
		}
		stdoutChan <- err
		logrus.Debugf("STDOUT/ERR copy completed")
	}()
	// Next, STDIN. Avoid entirely if attachStdin unset.
	if attachStdin {
		go func() {
			_, err := detach.Copy(conn, httpBuf, isDetach)
			logrus.Debugf("STDIN copy completed")
			stdinChan <- err
		}()
	}

	for {
		select {
		case err := <-stdoutChan:
			if err != nil {
				return err
			}

			return nil
		case err := <-stdinChan:
			if err != nil {
				return err
			}
			// copy stdin is done, close it
			if connErr := socketCloseWrite(conn); connErr != nil {
				logrus.Errorf("Unable to close conn: %v", connErr)
			}
		case <-cancel:
			return nil
		}
	}
}

// AttachResize resizes the terminal used by the given container.
func (r *ShimV2OCIRuntime) AttachResize(ctr *Container, newSize resize.TerminalSize) error {
	controlFile, err := openControlFile(ctr, ctr.bundlePath())
	if err != nil {
		return err
	}
	defer controlFile.Close()

	logrus.Debugf("Received a resize event for container %s: %+v", ctr.ID(), newSize)
	if _, err = fmt.Fprintf(controlFile, "%d %d %d\n", 1, newSize.Height, newSize.Width); err != nil {
		return fmt.Errorf("failed to write to ctl file to resize terminal: %w", err)
	}

	return nil
}

// CheckpointContainer checkpoints the given container.
func (r *ShimV2OCIRuntime) CheckpointContainer(ctr *Container, options ContainerCheckpointOptions) (int64, error) {
	// imagePath is used by CRIU to store the actual checkpoint files
	imagePath := ctr.CheckpointPath()
	if options.PreCheckPoint {
		imagePath = ctr.PreCheckPointPath()
	}
	// workPath will be used to store dump.log and stats-dump
	workPath := ctr.bundlePath()
	logrus.Debugf("Writing checkpoint to %s", imagePath)
	logrus.Debugf("Writing checkpoint logs to %s", workPath)
	logrus.Debugf("Pre-dump the container %t", options.PreCheckPoint)
	args := []string{}
	args = append(args, r.runtimeFlags...)
	args = append(args, "checkpoint")
	args = append(args, "--image-path")
	args = append(args, imagePath)
	args = append(args, "--work-path")
	args = append(args, workPath)
	if options.KeepRunning {
		args = append(args, "--leave-running")
	}
	if options.TCPEstablished {
		args = append(args, "--tcp-established")
	}
	if options.FileLocks {
		args = append(args, "--file-locks")
	}
	if !options.PreCheckPoint && options.KeepRunning {
		args = append(args, "--leave-running")
	}
	if options.PreCheckPoint {
		args = append(args, "--pre-dump")
	}
	if !options.PreCheckPoint && options.WithPrevious {
		args = append(
			args,
			"--parent-path",
			filepath.Join("..", preCheckpointDir),
		)
	}

	args = append(args, ctr.ID())
	logrus.Debugf("the args to checkpoint: %s %s", r.path, strings.Join(args, " "))

	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return 0, err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, fmt.Sprintf("PATH=%s", path))
	}

	var runtimeCheckpointStarted time.Time
	err = r.withContainerSocketLabel(ctr, func() error {
		runtimeCheckpointStarted = time.Now()
		return utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, args...)
	})

	runtimeCheckpointDuration := func() int64 {
		if options.PrintStats {
			return time.Since(runtimeCheckpointStarted).Microseconds()
		}
		return 0
	}()

	return runtimeCheckpointDuration, err
}

func (r *ShimV2OCIRuntime) CheckShimRunning(ctr *Container) (bool, error) {
	if ctr.state.ShimPID == 0 {
		// If the container is running or paused, assume ShimV2 is
		// running. We didn't record ShimV2 PID on some old versions, so
		// that is likely what's going on...
		// Unusual enough that we should print a warning message though.
		if ctr.ensureState(define.ContainerStateRunning, define.ContainerStatePaused) {
			logrus.Warnf("ShimV2 PID is not set, but container is running!")
			return true, nil
		}
		// Container's not running, so shimV2 PID being unset is
		// expected. ShimV2 is not running.
		return false, nil
	}

	// We have a shimV2 PID. Ping it with signal 0.
	if err := unix.Kill(ctr.state.ShimPID, 0); err != nil {
		if err == unix.ESRCH {
			return false, nil
		}
		return false, fmt.Errorf("pinging container %s shimV2 with signal 0: %w", ctr.ID(), err)
	}
	return true, nil
}

// SupportsCheckpoint checks if the OCI runtime supports checkpointing
// containers.
func (r *ShimV2OCIRuntime) SupportsCheckpoint() bool {
	return crutils.CRRuntimeSupportsCheckpointRestore(r.path)
}

// SupportsJSONErrors checks if the OCI runtime supports JSON-formatted error
// messages.
func (r *ShimV2OCIRuntime) SupportsJSONErrors() bool {
	return r.supportsJSON
}

// SupportsNoCgroups checks if the OCI runtime supports running containers
// without cgroups (the --cgroup-manager=disabled flag).
func (r *ShimV2OCIRuntime) SupportsNoCgroups() bool {
	return r.supportsNoCgroups
}

// SupportsKVM checks if the OCI runtime supports running containers
// without KVM separation
func (r *ShimV2OCIRuntime) SupportsKVM() bool {
	return r.supportsKVM
}

// AttachSocketPath is the path to a single container's attach socket.
func (r *ShimV2OCIRuntime) AttachSocketPath(ctr *Container) (string, error) {
	if ctr == nil {
		return "", fmt.Errorf("must provide a valid container to get attach socket path: %w", define.ErrInvalidArg)
	}

	return filepath.Join(ctr.bundlePath(), "attach"), nil
}

// ExitFilePath is the path to a container's exit file.
func (r *ShimV2OCIRuntime) ExitFilePath(ctr *Container) (string, error) {
	if ctr == nil {
		return "", fmt.Errorf("must provide a valid container to get exit file path: %w", define.ErrInvalidArg)
	}
	return filepath.Join(r.exitsDir, ctr.ID()), nil
}

// RuntimeInfo provides information on the runtime.
func (r *ShimV2OCIRuntime) RuntimeInfo() (*define.ShimInfo, *define.OCIRuntimeInfo, error) {
	runtimePackage := version.Package(r.path)
	runtimeVersion, err := r.getOCIRuntimeVersion()
	if err != nil {
		return nil, nil, fmt.Errorf("getting version of shim-v2 runtime %s: %w", r.name, err)
	}

	shimV2 := define.ShimInfo{
		Package: runtimePackage,
		Path:    r.path,
		Version: runtimeVersion,
	}
	ocirt := define.OCIRuntimeInfo{
		Name:    r.name,
		Path:    r.path,
		Package: runtimePackage,
		Version: runtimeVersion,
	}
	return &shimV2, &ocirt, nil
}

func (r *ShimV2OCIRuntime) getLogTag(ctr *Container) (string, error) {
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

// createOCIContainer generates this container's main shimV2 instance and prepares it for starting
func (r *ShimV2OCIRuntime) createOCIContainer(ctr *Container, restoreOptions *ContainerCheckpointOptions) (int64, error) {
	var stderrBuf bytes.Buffer

	parentSyncPipe, childSyncPipe, err := newPipe()
	if err != nil {
		return 0, fmt.Errorf("creating socket pair: %w", err)
	}
	defer errorhandling.CloseQuiet(parentSyncPipe)

	childStartPipe, parentStartPipe, err := newPipe()
	if err != nil {
		return 0, fmt.Errorf("creating socket pair for start pipe: %w", err)
	}

	defer errorhandling.CloseQuiet(parentStartPipe)

	var ociLog string
	if logrus.GetLevel() != logrus.DebugLevel && r.supportsJSON {
		ociLog = filepath.Join(ctr.state.RunDir, "oci-log")
	}

	logTag, err := r.getLogTag(ctr)
	if err != nil {
		return 0, err
	}

	if ctr.config.CgroupsMode == cgroupSplit {
		if err := moveToRuntimeCgroup(); err != nil {
			return 0, err
		}
	}

	pidfile := ctr.config.PidFile
	if pidfile == "" {
		pidfile = filepath.Join(ctr.state.RunDir, "pidfile")
	}

	args := r.sharedShimV2Args(ctr, ctr.ID(), ctr.bundlePath(), pidfile, ctr.LogPath(), r.exitsDir, ociLog, ctr.LogDriver(), logTag)

	if ctr.config.SdNotifyMode == define.SdNotifyModeContainer && ctr.config.SdNotifySocket != "" {
		args = append(args, fmt.Sprintf("--sdnotify-socket=%s", ctr.config.SdNotifySocket))
	}

	if ctr.Terminal() {
		args = append(args, "-t")
	} else if ctr.config.Stdin {
		args = append(args, "-i")
	}

	if ctr.config.Timeout > 0 {
		args = append(args, fmt.Sprintf("--timeout=%d", ctr.config.Timeout))
	}

	if !r.enableKeyring {
		args = append(args, "--no-new-keyring")
	}
	if ctr.config.ShimPidFile != "" {
		args = append(args, "--shimV2-pidfile", ctr.config.ShimPidFile)
	}

	if r.noPivot {
		args = append(args, "--no-pivot")
	}

	exitCommand, err := specgenutil.CreateExitCommandArgs(ctr.runtime.storageConfig, ctr.runtime.config, ctr.runtime.syslog || logrus.IsLevelEnabled(logrus.DebugLevel), ctr.AutoRemove(), false)
	if err != nil {
		return 0, err
	}
	exitCommand = append(exitCommand, ctr.config.ID)

	args = append(args, "--exit-command", exitCommand[0])
	for _, arg := range exitCommand[1:] {
		args = append(args, []string{"--exit-command-arg", arg}...)
	}

	preserveFDs := ctr.config.PreserveFDs

	// Pass down the LISTEN_* environment (see #10443).
	if val := os.Getenv("LISTEN_FDS"); val != "" {
		if preserveFDs > 0 || len(ctr.config.PreserveFD) > 0 {
			logrus.Warnf("Ignoring LISTEN_FDS to preserve custom user-specified FDs")
		} else {
			fds, err := strconv.Atoi(val)
			if err != nil {
				return 0, fmt.Errorf("converting LISTEN_FDS=%s: %w", val, err)
			}
			preserveFDs = uint(fds)
		}
	}

	preserveFDs, filesToClose, extraFiles, err := getPreserveFdExtraFiles(ctr.config.PreserveFD, preserveFDs)
	if err != nil {
		return 0, err
	}
	if preserveFDs > 0 {
		args = append(args, formatRuntimeOpts("--preserve-fds", strconv.FormatUint(uint64(preserveFDs), 10))...)
	}

	if restoreOptions != nil {
		args = append(args, "--restore", ctr.CheckpointPath())
		if restoreOptions.TCPEstablished {
			args = append(args, "--runtime-opt", "--tcp-established")
		}
		if restoreOptions.FileLocks {
			args = append(args, "--runtime-opt", "--file-locks")
		}
		if restoreOptions.Pod != "" {
			mountLabel := ctr.config.MountLabel
			processLabel := ctr.config.ProcessLabel
			if mountLabel != "" {
				args = append(
					args,
					"--runtime-opt",
					fmt.Sprintf(
						"--lsm-mount-context=%s",
						mountLabel,
					),
				)
			}
			if processLabel != "" {
				args = append(
					args,
					"--runtime-opt",
					fmt.Sprintf(
						"--lsm-profile=selinux:%s",
						processLabel,
					),
				)
			}
		}
	}

	logrus.WithFields(logrus.Fields{
		"args": args,
	}).Debugf("running shim V2 binary: %s", r.path)

	cmd := exec.Command(r.path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	// TODO this is probably a really bad idea for some uses
	// Make this configurable
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if ctr.Terminal() {
		cmd.Stderr = &stderrBuf
	}

	// 0, 1 and 2 are stdin, stdout and stderr
	shimV2Env, err := r.configureShimV2Env()
	if err != nil {
		return 0, fmt.Errorf("configuring shimV2 env: %w", err)
	}

	cmd.ExtraFiles = extraFiles

	cmd.Env = r.shimV2Env
	// we don't want to step on users fds they asked to preserve
	// Since 0-2 are used for stdio, start the fds we pass in at preserveFDs+3
	cmd.Env = append(cmd.Env, fmt.Sprintf("_OCI_SYNCPIPE=%d", preserveFDs+3), fmt.Sprintf("_OCI_STARTPIPE=%d", preserveFDs+4))
	cmd.Env = append(cmd.Env, shimV2Env...)
	cmd.ExtraFiles = append(cmd.ExtraFiles, childSyncPipe, childStartPipe)

	if r.reservePorts && !rootless.IsRootless() && !ctr.config.NetMode.IsSlirp4netns() {
		ports, err := bindPorts(ctr.convertPortMappings())
		if err != nil {
			return 0, err
		}
		filesToClose = append(filesToClose, ports...)

		// Leak the port we bound in the shimV2 process.  These fd's won't be used
		// by the container and shimV2 will keep the ports busy so that another
		// process cannot use them.
		cmd.ExtraFiles = append(cmd.ExtraFiles, ports...)
	}

	if ctr.config.NetMode.IsSlirp4netns() || rootless.IsRootless() {
		if ctr.config.PostConfigureNetNS {
			havePortMapping := len(ctr.config.PortMappings) > 0
			if havePortMapping {
				ctr.rootlessPortSyncR, ctr.rootlessPortSyncW, err = os.Pipe()
				if err != nil {
					return 0, fmt.Errorf("failed to create rootless port sync pipe: %w", err)
				}
			}
			ctr.rootlessSlirpSyncR, ctr.rootlessSlirpSyncW, err = os.Pipe()
			if err != nil {
				return 0, fmt.Errorf("failed to create rootless network sync pipe: %w", err)
			}
		}

		if ctr.rootlessSlirpSyncW != nil {
			defer errorhandling.CloseQuiet(ctr.rootlessSlirpSyncW)
			// Leak one end in shimV2, the other one will be leaked into slirp4netns
			cmd.ExtraFiles = append(cmd.ExtraFiles, ctr.rootlessSlirpSyncW)
		}

		if ctr.rootlessPortSyncW != nil {
			defer errorhandling.CloseQuiet(ctr.rootlessPortSyncW)
			// Leak one end in shimV2, the other one will be leaked into rootlessport
			cmd.ExtraFiles = append(cmd.ExtraFiles, ctr.rootlessPortSyncW)
		}
	}
	var runtimeRestoreStarted time.Time
	if restoreOptions != nil {
		runtimeRestoreStarted = time.Now()
	}
	err = cmd.Start()

	// regardless of whether we errored or not, we no longer need the children pipes
	childSyncPipe.Close()
	childStartPipe.Close()
	if err != nil {
		return 0, err
	}
	if err := r.moveShimV2ToCgroupAndSignal(ctr, cmd, parentStartPipe); err != nil {
		// The child likely already exited in which case the cmd.Wait() below should return the proper error.
		// EPIPE is expected if the child already exited so not worth to log and kill the process.
		if !errors.Is(err, syscall.EPIPE) {
			logrus.Errorf("Failed to signal shimV2 to start: %v", err)
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, syscall.ESRCH) {
				logrus.Errorf("Failed to kill shimV2 after error: %v", err)
			}
		}
	}

	/* Wait for initial setup and fork, and reap child */
	err = cmd.Wait()
	if err != nil {
		return 0, fmt.Errorf("shimV2 failed: %w", err)
	}

	pid, err := readShimPipeData(r.name, parentSyncPipe, ociLog)
	if err != nil {
		if err2 := r.DeleteContainer(ctr); err2 != nil {
			logrus.Errorf("Removing container %s from runtime after creation failed", ctr.ID())
		}
		return 0, err
	}
	ctr.state.PID = pid

	shimV2PID, err := readShimPidFile(ctr.config.ShimPidFile)
	if err != nil {
		logrus.Warnf("Error reading shimV2 pid file for container %s: %v", ctr.ID(), err)
	} else if shimV2PID > 0 {
		// shimV2 not having a pid file is a valid state, so don't set it if we don't have it
		logrus.Infof("Got shim V2 PID as %d", shimV2PID)
		ctr.state.ShimPID = shimV2PID
	}

	runtimeRestoreDuration := func() int64 {
		if restoreOptions != nil && restoreOptions.PrintStats {
			return time.Since(runtimeRestoreStarted).Microseconds()
		}
		return 0
	}()

	// These fds were passed down to the runtime.  Close them
	// and not interfere
	for _, f := range filesToClose {
		errorhandling.CloseQuiet(f)
	}

	return runtimeRestoreDuration, nil
}

// configureShimV2Env gets the environment values to add to shimV2's exec struct
func (r *ShimV2OCIRuntime) configureShimV2Env() ([]string, error) {
	env := os.Environ()
	res := make([]string, 0, len(env))
	for _, v := range env {
		if strings.HasPrefix(v, "NOTIFY_SOCKET=") {
			// The NOTIFY_SOCKET must not leak into the environment.
			continue
		}
		if strings.HasPrefix(v, "DBUS_SESSION_BUS_ADDRESS=") && !rootless.IsRootless() {
			// The DBUS_SESSION_BUS_ADDRESS must not leak into the environment when running as root.
			// This is because we want to use the system session for root containers, not the user session.
			continue
		}
		res = append(res, v)
	}
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return nil, err
	}

	res = append(res, "XDG_RUNTIME_DIR="+runtimeDir)
	return res, nil
}

// sharedShimV2Args takes common arguments for exec and create/restore and formats them for the shimV2 CLI
func (r *ShimV2OCIRuntime) sharedShimV2Args(ctr *Container, cuuid, bundlePath, pidPath, logPath, exitDir, ociLogPath, logDriver, logTag string) []string {
	// set the shimV2 API version to be able to use the correct sync struct keys
	args := []string{
		"--api-version", "1",
		"-c", ctr.ID(),
		"-u", cuuid,
		"-r", r.path,
		"-b", bundlePath,
		"-p", pidPath,
		"-n", ctr.Name(),
		"--exit-dir", exitDir,
		"--full-attach",
	}
	if len(r.runtimeFlags) > 0 {
		rFlags := []string{}
		for _, arg := range r.runtimeFlags {
			rFlags = append(rFlags, "--runtime-arg", arg)
		}
		args = append(args, rFlags...)
	}

	if ctr.CgroupManager() == config.SystemdCgroupsManager && !ctr.config.NoCgroups && ctr.config.CgroupsMode != cgroupSplit {
		args = append(args, "-s")
	}

	var logDriverArg string
	switch logDriver {
	case define.JournaldLogging:
		logDriverArg = define.JournaldLogging
	case define.NoLogging:
		logDriverArg = define.NoLogging
	case define.PassthroughLogging:
		logDriverArg = define.PassthroughLogging
	//lint:ignore ST1015 the default case has to be here
	default: //nolint:gocritic
		// No case here should happen except JSONLogging, but keep this here in case the options are extended
		logrus.Errorf("%s logging specified but not supported. Choosing k8s-file logging instead", ctr.LogDriver())
		fallthrough
	case "":
		// to get here, either a user would specify `--log-driver ""`, or this came from another place in libpod
		// since the former case is obscure, and the latter case isn't an error, let's silently fallthrough
		fallthrough
	case define.JSONLogging:
		fallthrough
	case define.KubernetesLogging:
		logDriverArg = fmt.Sprintf("%s:%s", define.KubernetesLogging, logPath)
	}

	args = append(args, "-l", logDriverArg)
	logLevel := logrus.GetLevel()
	args = append(args, "--log-level", logLevel.String())

	logrus.Debugf("%s messages will be logged to syslog", r.path)
	args = append(args, "--syslog")

	size := r.logSizeMax
	if ctr.config.LogSize > 0 {
		size = ctr.config.LogSize
	}
	if size > 0 {
		args = append(args, "--log-size-max", strconv.FormatInt(size, 10))
	}

	if ociLogPath != "" {
		args = append(args, "--runtime-arg", "--log-format=json", "--runtime-arg", "--log", fmt.Sprintf("--runtime-arg=%s", ociLogPath))
	}
	if logTag != "" {
		args = append(args, "--log-tag", logTag)
	}
	if ctr.config.NoCgroups {
		logrus.Debugf("Running with no Cgroups")
		args = append(args, "--runtime-arg", "--cgroup-manager", "--runtime-arg", "disabled")
	}
	return args
}

// readShimPidFile attempts to read shimV2's pid from its pid file
func readShimPidFile(pidFile string) (int, error) {
	// Let's try reading the ShimV2 pid at the same time.
	if pidFile != "" {
		contents, err := os.ReadFile(pidFile)
		if err != nil {
			return -1, err
		}
		// Convert it to an int
		shimV2PID, err := strconv.Atoi(string(contents))
		if err != nil {
			return -1, err
		}
		return shimV2PID, nil
	}
	return 0, nil
}

// readShimPipeData attempts to read a syncInfo struct from the pipe
func readShimPipeData(runtimeName string, pipe *os.File, ociLog string) (int, error) {
	// syncInfo is used to return data from monitor process to daemon
	type syncInfo struct {
		Data    int    `json:"data"`
		Message string `json:"message,omitempty"`
	}

	// Wait to get container pid from shimV2
	type syncStruct struct {
		si  *syncInfo
		err error
	}
	ch := make(chan syncStruct)
	go func() {
		var si *syncInfo
		rdr := bufio.NewReader(pipe)
		b, err := rdr.ReadBytes('\n')
		// ignore EOF here, error is returned even when data was read
		// if it is no valid json unmarshal will fail below
		if err != nil && !errors.Is(err, io.EOF) {
			ch <- syncStruct{err: err}
		}
		if err := json.Unmarshal(b, &si); err != nil {
			ch <- syncStruct{err: fmt.Errorf("shimV2 bytes %q: %w", string(b), err)}
			return
		}
		ch <- syncStruct{si: si}
	}()

	var data int
	select {
	case ss := <-ch:
		if ss.err != nil {
			if ociLog != "" {
				ociLogData, err := os.ReadFile(ociLog)
				if err == nil {
					var ociErr ociError
					if err := json.Unmarshal(ociLogData, &ociErr); err == nil {
						return -1, getOCIRuntimeError(runtimeName, ociErr.Msg)
					}
				}
			}
			return -1, fmt.Errorf("container create failed (no logs from shimV2): %w", ss.err)
		}
		logrus.Debugf("Received: %d", ss.si.Data)
		if ss.si.Data < 0 {
			if ociLog != "" {
				ociLogData, err := os.ReadFile(ociLog)
				if err == nil {
					var ociErr ociError
					if err := json.Unmarshal(ociLogData, &ociErr); err == nil {
						return ss.si.Data, getOCIRuntimeError(runtimeName, ociErr.Msg)
					}
				}
			}
			// If we failed to parse the JSON errors, then print the output as it is
			if ss.si.Message != "" {
				return ss.si.Data, getOCIRuntimeError(runtimeName, ss.si.Message)
			}
			return ss.si.Data, fmt.Errorf("container create failed: %w", define.ErrInternal)
		}
		data = ss.si.Data
	case <-time.After(define.ContainerCreateTimeout):
		return -1, fmt.Errorf("container creation timeout: %w", define.ErrInternal)
	}
	return data, nil
}

// writeShimPipeData writes nonce data to a pipe
func writeShimPipeData(pipe *os.File) error {
	someData := []byte{0}
	_, err := pipe.Write(someData)
	return err
}

// getShimV2Version returns a string representation of the shimV2 version.
func (r *ShimV2OCIRuntime) getShimV2Version() (string, error) {
	output, err := utils.ExecCmd(r.path, "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.Replace(output, "\n", ", ", 1), "\n"), nil
}

// getOCIRuntimeVersion returns a string representation of the OCI runtime's
// version.
func (r *ShimV2OCIRuntime) getOCIRuntimeVersion() (string, error) {
	output, err := utils.ExecCmd(r.path, "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(output, "\n"), nil
}

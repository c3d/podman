//go:build !remote && (linux || freebsd)

package libpod

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containers/common/pkg/config"
	"github.com/containers/common/pkg/detach"
	"github.com/containers/common/pkg/resize"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/containers/podman/v4/pkg/errorhandling"
	"github.com/containers/podman/v4/pkg/lookup"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// ExecContainer executes a command in a running container
func (r *ShimV2OCIRuntime) ExecContainer(c *Container, sessionID string, options *ExecOptions, streams *define.AttachStreams, newSize *resize.TerminalSize) (int, chan error, error) {
	if options == nil {
		return -1, nil, fmt.Errorf("must provide an ExecOptions struct to ExecContainer: %w", define.ErrInvalidArg)
	}
	if len(options.Cmd) == 0 {
		return -1, nil, fmt.Errorf("must provide a command to execute: %w", define.ErrInvalidArg)
	}

	if sessionID == "" {
		return -1, nil, fmt.Errorf("must provide a session ID for exec: %w", define.ErrEmptyID)
	}

	// TODO: Should we default this to false?
	// Or maybe make streams mandatory?
	attachStdin := true
	if streams != nil {
		attachStdin = streams.AttachInput
	}

	var ociLog string
	if logrus.GetLevel() != logrus.DebugLevel && r.supportsJSON {
		ociLog = c.execOCILog(sessionID)
	}

	execCmd, pipes, err := r.startExec(c, sessionID, options, attachStdin, ociLog)
	if err != nil {
		return -1, nil, err
	}

	// Only close sync pipe. Start and attach are consumed in the attach
	// goroutine.
	defer func() {
		if pipes.syncPipe != nil && !pipes.syncClosed {
			errorhandling.CloseQuiet(pipes.syncPipe)
			pipes.syncClosed = true
		}
	}()

	// TODO Only create if !detach
	// Attach to the container before starting it
	attachChan := make(chan error)
	go func() {
		// attachToExec is responsible for closing pipes
		attachChan <- c.attachToExec(streams, options.DetachKeys, sessionID, pipes.startPipe, pipes.attachPipe, newSize)
		close(attachChan)
	}()

	if err := execCmd.Wait(); err != nil {
		return -1, nil, fmt.Errorf("cannot run shimV2: %w", err)
	}

	pid, err := readShimPipeData(r.name, pipes.syncPipe, ociLog)

	return pid, attachChan, err
}

// ExecContainerHTTP executes a new command in an existing container and
// forwards its standard streams over an attach
func (r *ShimV2OCIRuntime) ExecContainerHTTP(ctr *Container, sessionID string, options *ExecOptions, req *http.Request, w http.ResponseWriter,
	streams *HTTPAttachStreams, cancel <-chan bool, hijackDone chan<- bool, holdConnOpen <-chan bool, newSize *resize.TerminalSize) (int, chan error, error) {
	if streams != nil {
		if !streams.Stdin && !streams.Stdout && !streams.Stderr {
			return -1, nil, fmt.Errorf("must provide at least one stream to attach to: %w", define.ErrInvalidArg)
		}
	}

	if options == nil {
		return -1, nil, fmt.Errorf("must provide exec options to ExecContainerHTTP: %w", define.ErrInvalidArg)
	}

	detachString := config.DefaultDetachKeys
	if options.DetachKeys != nil {
		detachString = *options.DetachKeys
	}
	detachKeys, err := processDetachKeys(detachString)
	if err != nil {
		return -1, nil, err
	}

	// TODO: Should we default this to false?
	// Or maybe make streams mandatory?
	attachStdin := true
	if streams != nil {
		attachStdin = streams.Stdin
	}

	var ociLog string
	if logrus.GetLevel() != logrus.DebugLevel && r.supportsJSON {
		ociLog = ctr.execOCILog(sessionID)
	}

	execCmd, pipes, err := r.startExec(ctr, sessionID, options, attachStdin, ociLog)
	if err != nil {
		return -1, nil, err
	}

	// Only close sync pipe. Start and attach are consumed in the attach
	// goroutine.
	defer func() {
		if pipes.syncPipe != nil && !pipes.syncClosed {
			errorhandling.CloseQuiet(pipes.syncPipe)
			pipes.syncClosed = true
		}
	}()

	attachChan := make(chan error)
	shimPipeDataChan := make(chan shimPipeData)
	go func() {
		// attachToExec is responsible for closing pipes
		attachChan <- attachExecHTTP(ctr, sessionID, req, w, streams, pipes, detachKeys, options.Terminal, cancel, hijackDone, holdConnOpen, execCmd, shimPipeDataChan, ociLog, newSize, r.name)
		close(attachChan)
	}()

	// NOTE: the channel is needed to communicate shimV2's data.  In case
	// of an error, the error will be written on the hijacked http
	// connection such that remote clients will receive the error.
	pipeData := <-shimPipeDataChan

	return pipeData.pid, attachChan, pipeData.err
}

// ExecContainerDetached executes a command in a running container, but does
// not attach to it.
func (r *ShimV2OCIRuntime) ExecContainerDetached(ctr *Container, sessionID string, options *ExecOptions, stdin bool) (int, error) {
	if options == nil {
		return -1, fmt.Errorf("must provide exec options to ExecContainerHTTP: %w", define.ErrInvalidArg)
	}

	var ociLog string
	if logrus.GetLevel() != logrus.DebugLevel && r.supportsJSON {
		ociLog = ctr.execOCILog(sessionID)
	}

	execCmd, pipes, err := r.startExec(ctr, sessionID, options, stdin, ociLog)
	if err != nil {
		return -1, err
	}

	defer func() {
		pipes.cleanup()
	}()

	// Wait for ShimV2 to tell us we're ready to attach.
	// We aren't actually *going* to attach, but this means that we're good
	// to proceed.
	if _, err := readShimPipeData(r.name, pipes.attachPipe, ""); err != nil {
		return -1, err
	}

	// Start the exec session
	if err := writeShimPipeData(pipes.startPipe); err != nil {
		return -1, err
	}

	// Wait for shimV2 to succeed, when return.
	if err := execCmd.Wait(); err != nil {
		return -1, fmt.Errorf("cannot run shimV2: %w", err)
	}

	pid, err := readShimPipeData(r.name, pipes.syncPipe, ociLog)

	return pid, err
}

// ExecAttachResize resizes the TTY of the given exec session.
func (r *ShimV2OCIRuntime) ExecAttachResize(ctr *Container, sessionID string, newSize resize.TerminalSize) error {
	controlFile, err := openControlFile(ctr, ctr.execBundlePath(sessionID))
	if err != nil {
		return err
	}
	defer controlFile.Close()

	if _, err = fmt.Fprintf(controlFile, "%d %d %d\n", 1, newSize.Height, newSize.Width); err != nil {
		return fmt.Errorf("failed to write to ctl file to resize terminal: %w", err)
	}

	return nil
}

// ExecStopContainer stops a given exec session in a running container.
func (r *ShimV2OCIRuntime) ExecStopContainer(ctr *Container, sessionID string, timeout uint) error {
	pid, err := ctr.getExecSessionPID(sessionID)
	if err != nil {
		return err
	}

	logrus.Debugf("Going to stop container %s exec session %s", ctr.ID(), sessionID)

	// Is the session dead?
	// Ping the PID with signal 0 to see if it still exists.
	if err := unix.Kill(pid, 0); err != nil {
		if err == unix.ESRCH {
			return nil
		}
		return fmt.Errorf("pinging container %s exec session %s PID %d with signal 0: %w", ctr.ID(), sessionID, pid, err)
	}

	if timeout > 0 {
		// Use SIGTERM by default, then SIGSTOP after timeout.
		logrus.Debugf("Killing exec session %s (PID %d) of container %s with SIGTERM", sessionID, pid, ctr.ID())
		if err := unix.Kill(pid, unix.SIGTERM); err != nil {
			if err == unix.ESRCH {
				return nil
			}
			return fmt.Errorf("killing container %s exec session %s PID %d with SIGTERM: %w", ctr.ID(), sessionID, pid, err)
		}

		// Wait for the PID to stop
		if err := waitPidStop(pid, time.Duration(timeout)*time.Second); err != nil {
			logrus.Infof("Timed out waiting for container %s exec session %s to stop, resorting to SIGKILL: %v", ctr.ID(), sessionID, err)
		} else {
			// No error, container is dead
			return nil
		}
	}

	// SIGTERM did not work. On to SIGKILL.
	logrus.Debugf("Killing exec session %s (PID %d) of container %s with SIGKILL", sessionID, pid, ctr.ID())
	if err := unix.Kill(pid, unix.SIGTERM); err != nil {
		if err == unix.ESRCH {
			return nil
		}
		return fmt.Errorf("killing container %s exec session %s PID %d with SIGKILL: %w", ctr.ID(), sessionID, pid, err)
	}

	// Wait for the PID to stop
	if err := waitPidStop(pid, killContainerTimeout); err != nil {
		return fmt.Errorf("timed out waiting for container %s exec session %s PID %d to stop after SIGKILL: %w", ctr.ID(), sessionID, pid, err)
	}

	return nil
}

// ExecUpdateStatus checks if the given exec session is still running.
func (r *ShimV2OCIRuntime) ExecUpdateStatus(ctr *Container, sessionID string) (bool, error) {
	pid, err := ctr.getExecSessionPID(sessionID)
	if err != nil {
		return false, err
	}

	logrus.Debugf("Checking status of container %s exec session %s", ctr.ID(), sessionID)

	// Is the session dead?
	// Ping the PID with signal 0 to see if it still exists.
	if err := unix.Kill(pid, 0); err != nil {
		if err == unix.ESRCH {
			return false, nil
		}
		return false, fmt.Errorf("pinging container %s exec session %s PID %d with signal 0: %w", ctr.ID(), sessionID, pid, err)
	}

	return true, nil
}

// ExecAttachSocketPath is the path to a container's exec session attach socket.
func (r *ShimV2OCIRuntime) ExecAttachSocketPath(ctr *Container, sessionID string) (string, error) {
	// We don't even use container, so don't validity check it
	if sessionID == "" {
		return "", fmt.Errorf("must provide a valid session ID to get attach socket path: %w", define.ErrInvalidArg)
	}

	return filepath.Join(ctr.execBundlePath(sessionID), "attach"), nil
}

// Start an exec session's shimV2 parent from the given options.
func (r *ShimV2OCIRuntime) startExec(c *Container, sessionID string, options *ExecOptions, attachStdin bool, ociLog string) (_ *exec.Cmd, _ *execPipes, deferredErr error) {
	pipes := new(execPipes)

	if options == nil {
		return nil, nil, fmt.Errorf("must provide an ExecOptions struct to ExecContainer: %w", define.ErrInvalidArg)
	}
	if len(options.Cmd) == 0 {
		return nil, nil, fmt.Errorf("must provide a command to execute: %w", define.ErrInvalidArg)
	}

	if sessionID == "" {
		return nil, nil, fmt.Errorf("must provide a session ID for exec: %w", define.ErrEmptyID)
	}

	// create sync pipe to receive the pid
	parentSyncPipe, childSyncPipe, err := newPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("creating socket pair: %w", err)
	}
	pipes.syncPipe = parentSyncPipe

	defer func() {
		if deferredErr != nil {
			pipes.cleanup()
		}
	}()

	// create start pipe to set the cgroup before running
	// attachToExec is responsible for closing parentStartPipe
	childStartPipe, parentStartPipe, err := newPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("creating socket pair: %w", err)
	}
	pipes.startPipe = parentStartPipe

	// create the attach pipe to allow attach socket to be created before
	// $RUNTIME exec starts running. This is to make sure we can capture all output
	// from the process through that socket, rather than half reading the log, half attaching to the socket
	// attachToExec is responsible for closing parentAttachPipe
	parentAttachPipe, childAttachPipe, err := newPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("creating socket pair: %w", err)
	}
	pipes.attachPipe = parentAttachPipe

	childrenClosed := false
	defer func() {
		if !childrenClosed {
			errorhandling.CloseQuiet(childSyncPipe)
			errorhandling.CloseQuiet(childAttachPipe)
			errorhandling.CloseQuiet(childStartPipe)
		}
	}()

	finalEnv := make([]string, 0, len(options.Env))
	for k, v := range options.Env {
		finalEnv = append(finalEnv, fmt.Sprintf("%s=%s", k, v))
	}

	processFile, err := c.prepareProcessExec(options, finalEnv, sessionID)
	if err != nil {
		return nil, nil, err
	}
	defer processFile.Close()

	args := r.sharedShimV2Args(c, sessionID, c.execBundlePath(sessionID), c.execPidPath(sessionID), c.execLogPath(sessionID), c.execExitFileDir(sessionID), ociLog, define.NoLogging, c.config.LogTag)

	preserveFDs, filesToClose, extraFiles, err := getPreserveFdExtraFiles(options.PreserveFD, options.PreserveFDs)
	if err != nil {
		return nil, nil, err
	}

	if preserveFDs > 0 {
		args = append(args, formatRuntimeOpts("--preserve-fds", strconv.FormatUint(uint64(preserveFDs), 10))...)
	}

	if options.Terminal {
		args = append(args, "-t")
	}

	if attachStdin {
		args = append(args, "-i")
	}

	// Append container ID and command
	args = append(args, "-e")
	// TODO make this optional when we can detach
	args = append(args, "--exec-attach")
	args = append(args, "--exec-process-spec", processFile.Name())

	if len(options.ExitCommand) > 0 {
		args = append(args, "--exit-command", options.ExitCommand[0])
		for _, arg := range options.ExitCommand[1:] {
			args = append(args, []string{"--exit-command-arg", arg}...)
		}
		if options.ExitCommandDelay > 0 {
			args = append(args, []string{"--exit-delay", strconv.FormatUint(uint64(options.ExitCommandDelay), 10)}...)
		}
	}

	logrus.WithFields(logrus.Fields{
		"args": args,
	}).Debugf("running shim V2 binary: %s", r.path)
	execCmd := exec.Command(r.path, args...)

	// TODO: This is commented because it doesn't make much sense in HTTP
	// attach, and I'm not certain it does for non-HTTP attach as well.
	// if streams != nil {
	// 	// Don't add the InputStream to the execCmd. Instead, the data should be passed
	// 	// through CopyDetachable
	// 	if streams.AttachOutput {
	// 		execCmd.Stdout = options.Streams.OutputStream
	// 	}
	// 	if streams.AttachError {
	// 		execCmd.Stderr = options.Streams.ErrorStream
	// 	}
	// }

	shimV2Env, err := r.configureShimV2Env()
	if err != nil {
		return nil, nil, fmt.Errorf("configuring shimV2 env: %w", err)
	}

	execCmd.ExtraFiles = extraFiles

	// we don't want to step on users fds they asked to preserve
	// Since 0-2 are used for stdio, start the fds we pass in at preserveFDs+3
	execCmd.Env = r.shimV2Env
	execCmd.Env = append(execCmd.Env, fmt.Sprintf("_OCI_SYNCPIPE=%d", preserveFDs+3), fmt.Sprintf("_OCI_STARTPIPE=%d", preserveFDs+4), fmt.Sprintf("_OCI_ATTACHPIPE=%d", preserveFDs+5))
	execCmd.Env = append(execCmd.Env, shimV2Env...)

	execCmd.ExtraFiles = append(execCmd.ExtraFiles, childSyncPipe, childStartPipe, childAttachPipe)
	execCmd.Dir = c.execBundlePath(sessionID)
	execCmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	err = execCmd.Start()

	// We don't need children pipes  on the parent side
	errorhandling.CloseQuiet(childSyncPipe)
	errorhandling.CloseQuiet(childAttachPipe)
	errorhandling.CloseQuiet(childStartPipe)
	childrenClosed = true

	if err != nil {
		return nil, nil, fmt.Errorf("cannot start container %s: %w", c.ID(), err)
	}
	if err := r.moveShimV2ToCgroupAndSignal(c, execCmd, parentStartPipe); err != nil {
		return nil, nil, err
	}

	// These fds were passed down to the runtime.  Close them
	// and not interfere
	for _, f := range filesToClose {
		errorhandling.CloseQuiet(f)
	}

	return execCmd, pipes, nil
}

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

// This contains pipes used by the exec API.
type execPipes struct {
	syncPipe     *os.File
	syncClosed   bool
	startPipe    *os.File
	startClosed  bool
	attachPipe   *os.File
	attachClosed bool
}

func (p *execPipes) cleanup() {
	if p.syncPipe != nil && !p.syncClosed {
		errorhandling.CloseQuiet(p.syncPipe)
		p.syncClosed = true
	}
	if p.startPipe != nil && !p.startClosed {
		errorhandling.CloseQuiet(p.startPipe)
		p.startClosed = true
	}
	if p.attachPipe != nil && !p.attachClosed {
		errorhandling.CloseQuiet(p.attachPipe)
		p.attachClosed = true
	}
}

// shimPipeData contains the data when reading from conmon's pipe.
type shimPipeData struct {
	pid int
	err error
}

// Attach to a container over HTTP
func attachExecHTTP(c *Container, sessionID string, r *http.Request, w http.ResponseWriter, streams *HTTPAttachStreams, pipes *execPipes, detachKeys []byte, isTerminal bool, cancel <-chan bool, hijackDone chan<- bool, holdConnOpen <-chan bool, execCmd *exec.Cmd, shimPipeDataChan chan<- shimPipeData, ociLog string, newSize *resize.TerminalSize, runtimeName string) (deferredErr error) {
	// NOTE: As you may notice, the attach code is quite complex.
	// Many things happen concurrently and yet are interdependent.
	// If you ever change this function, make sure to write to the
	// shimPipeDataChan in case of an error.

	if pipes == nil || pipes.startPipe == nil || pipes.attachPipe == nil {
		err := fmt.Errorf("must provide a start and attach pipe to finish an exec attach: %w", define.ErrInvalidArg)
		shimPipeDataChan <- shimPipeData{-1, err}
		return err
	}

	defer func() {
		if !pipes.startClosed {
			errorhandling.CloseQuiet(pipes.startPipe)
			pipes.startClosed = true
		}
		if !pipes.attachClosed {
			errorhandling.CloseQuiet(pipes.attachPipe)
			pipes.attachClosed = true
		}
	}()

	logrus.Debugf("Attaching to container %s exec session %s", c.ID(), sessionID)

	// set up the socket path, such that it is the correct length and location for exec
	sockPath, err := c.execAttachSocketPath(sessionID)
	if err != nil {
		shimPipeDataChan <- shimPipeData{-1, err}
		return err
	}

	// 2: read from attachFd that the parent process has set up the console socket
	if _, err := readShimPipeData(runtimeName, pipes.attachPipe, ""); err != nil {
		shimPipeDataChan <- shimPipeData{-1, err}
		return err
	}

	// resize before we start the container process
	if newSize != nil {
		err = c.ociRuntime.ExecAttachResize(c, sessionID, *newSize)
		if err != nil {
			logrus.Warnf("Resize failed: %v", err)
		}
	}

	// 2: then attach
	conn, err := openUnixSocket(sockPath)
	if err != nil {
		shimPipeDataChan <- shimPipeData{-1, err}
		return fmt.Errorf("failed to connect to container's attach socket: %v: %w", sockPath, err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			logrus.Errorf("Unable to close socket: %q", err)
		}
	}()

	attachStdout := true
	attachStderr := true
	attachStdin := true
	if streams != nil {
		attachStdout = streams.Stdout
		attachStderr = streams.Stderr
		attachStdin = streams.Stdin
	}

	// Perform hijack
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		shimPipeDataChan <- shimPipeData{-1, err}
		return errors.New("unable to hijack connection")
	}

	httpCon, httpBuf, err := hijacker.Hijack()
	if err != nil {
		shimPipeDataChan <- shimPipeData{-1, err}
		return fmt.Errorf("hijacking connection: %w", err)
	}

	hijackDone <- true

	// Write a header to let the client know what happened
	writeHijackHeader(r, httpBuf, isTerminal)

	// Force a flush after the header is written.
	if err := httpBuf.Flush(); err != nil {
		shimPipeDataChan <- shimPipeData{-1, err}
		return fmt.Errorf("flushing HTTP hijack header: %w", err)
	}

	go func() {
		// Wait for shimV2 to succeed, when return.
		if err := execCmd.Wait(); err != nil {
			shimPipeDataChan <- shimPipeData{-1, err}
		} else {
			pid, err := readShimPipeData(runtimeName, pipes.syncPipe, ociLog)
			if err != nil {
				hijackWriteError(err, c.ID(), isTerminal, httpBuf)
				shimPipeDataChan <- shimPipeData{pid, err}
			} else {
				shimPipeDataChan <- shimPipeData{pid, err}
			}
		}
		// We need to hold the connection open until the complete exec
		// function has finished. This channel will be closed in a defer
		// in that function, so we can wait for it here.
		// Can't be a defer, because this would block the function from
		// returning.
		<-holdConnOpen
		hijackWriteErrorAndClose(deferredErr, c.ID(), isTerminal, httpCon, httpBuf)
	}()

	stdoutChan := make(chan error)
	stdinChan := make(chan error)

	// Next, STDIN. Avoid entirely if attachStdin unset.
	if attachStdin {
		go func() {
			logrus.Debugf("Beginning STDIN copy")
			_, err := detach.Copy(conn, httpBuf, detachKeys)
			logrus.Debugf("STDIN copy completed")
			stdinChan <- err
		}()
	}

	// 4: send start message to child
	if err := writeShimPipeData(pipes.startPipe); err != nil {
		return err
	}

	// Handle STDOUT/STDERR *after* start message is sent
	go func() {
		var err error
		if isTerminal {
			// Hack: return immediately if attachStdout not set to
			// emulate Docker.
			// Basically, when terminal is set, STDERR goes nowhere.
			// Everything does over STDOUT.
			// Therefore, if not attaching STDOUT - we'll never copy
			// anything from here.
			logrus.Debugf("Performing terminal HTTP attach for container %s", c.ID())
			if attachStdout {
				err = httpAttachTerminalCopy(conn, httpBuf, c.ID())
			}
		} else {
			logrus.Debugf("Performing non-terminal HTTP attach for container %s", c.ID())
			err = httpAttachNonTerminalCopy(conn, httpBuf, c.ID(), attachStdin, attachStdout, attachStderr)
		}
		stdoutChan <- err
		logrus.Debugf("STDOUT/ERR copy completed")
	}()

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


// prepareProcessExec returns the path of the process.json used in runc exec -p
// caller is responsible to close the returned *os.File if needed.
func (c *Container) prepareProcessExec(options *ExecOptions, env []string, sessionID string) (*os.File, error) {
	f, err := os.CreateTemp(c.execBundlePath(sessionID), "exec-process-")
	if err != nil {
		return nil, err
	}
	pspec := new(spec.Process)
	if err := JSONDeepCopy(c.config.Spec.Process, pspec); err != nil {
		return nil, err
	}
	pspec.SelinuxLabel = c.config.ProcessLabel
	pspec.Args = options.Cmd

	// We need to default this to false else it will inherit terminal as true
	// from the container.
	pspec.Terminal = false
	if options.Terminal {
		pspec.Terminal = true
	}
	if len(env) > 0 {
		pspec.Env = append(pspec.Env, env...)
	}

	// Add secret envs if they exist
	manager, err := c.runtime.SecretsManager()
	if err != nil {
		return nil, err
	}
	for name, secr := range c.config.EnvSecrets {
		_, data, err := manager.LookupSecretData(secr.Name)
		if err != nil {
			return nil, err
		}
		pspec.Env = append(pspec.Env, fmt.Sprintf("%s=%s", name, string(data)))
	}

	if options.Cwd != "" {
		pspec.Cwd = options.Cwd
	}

	var addGroups []string
	var sgids []uint32

	// if the user is empty, we should inherit the user that the container is currently running with
	user := options.User
	if user == "" {
		logrus.Debugf("Set user to %s", c.config.User)
		user = c.config.User
		addGroups = c.config.Groups
	}

	overrides := c.getUserOverrides()
	execUser, err := lookup.GetUserGroupInfo(c.state.Mountpoint, user, overrides)
	if err != nil {
		return nil, err
	}

	if len(addGroups) > 0 {
		sgids, err = lookup.GetContainerGroups(addGroups, c.state.Mountpoint, overrides)
		if err != nil {
			return nil, fmt.Errorf("looking up supplemental groups for container %s exec session %s: %w", c.ID(), sessionID, err)
		}
	}

	// If user was set, look it up in the container to get a UID to use on
	// the host
	if user != "" || len(sgids) > 0 {
		if user != "" {
			for _, sgid := range execUser.Sgids {
				sgids = append(sgids, uint32(sgid))
			}
		}
		processUser := spec.User{
			UID:            uint32(execUser.Uid),
			GID:            uint32(execUser.Gid),
			AdditionalGids: sgids,
		}

		pspec.User = processUser
	}

	if c.config.Umask != "" {
		umask, err := c.umask()
		if err != nil {
			return nil, err
		}
		pspec.User.Umask = &umask
	}

	if err := c.setProcessCapabilitiesExec(options, user, execUser, pspec); err != nil {
		return nil, err
	}

	hasHomeSet := false
	for _, s := range pspec.Env {
		if strings.HasPrefix(s, "HOME=") {
			hasHomeSet = true
			break
		}
	}
	if !hasHomeSet {
		pspec.Env = append(pspec.Env, fmt.Sprintf("HOME=%s", execUser.Home))
	}

	processJSON, err := json.Marshal(pspec)
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(f.Name(), processJSON, 0644); err != nil {
		return nil, err
	}
	return f, nil
}

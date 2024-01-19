//go:build !remote && (linux || freebsd)

package libpod

import (
	"context"
	"fmt"
	"net"

	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v5/libpod/define"
	"github.com/sirupsen/logrus"
)

// Attach to the given container.
// Does not check if state is appropriate.
// started is only required if startContainer is true.
func (r *ShimV2OCIRuntime) Attach(c *Container, params *AttachOptions) error {
	passthrough := c.LogDriver() == define.PassthroughLogging

	if params == nil || params.Streams == nil {
		return fmt.Errorf("must provide parameters to Attach: %w", define.ErrInternal)
	}

	if !params.Streams.AttachOutput && !params.Streams.AttachError && !params.Streams.AttachInput && !passthrough {
		return fmt.Errorf("must provide at least one stream to attach to: %w", define.ErrInvalidArg)
	}
	if params.Start && params.Started == nil {
		return fmt.Errorf("started chan not passed when startContainer set: %w", define.ErrInternal)
	}

	keys := config.DefaultDetachKeys
	if params.DetachKeys != nil {
		keys = *params.DetachKeys
	}

	detachKeys, err := processDetachKeys(keys)
	if err != nil {
		return err
	}

	var conn *net.UnixConn
	if !passthrough {
		logrus.Debugf("Attaching to container %s", c.ID())

		// If we have a resize, do it.
		if params.InitialSize != nil {
			if err := r.AttachResize(c, *params.InitialSize); err != nil {
				return err
			}
		}

		attachSock, err := c.AttachSocketPath()
		if err != nil {
			return err
		}

		conn, err = openUnixSocket(attachSock)
		if err != nil {
			return fmt.Errorf("failed to connect to container's attach socket: %v: %w", attachSock, err)
		}
		defer func() {
			if err := conn.Close(); err != nil {
				logrus.Errorf("unable to close socket: %q", err)
			}
		}()
	}

	// If starting was requested, start the container and notify when that's
	// done.
	if params.Start {
		if err := c.start(context.TODO()); err != nil {
			return err
		}
		params.Started <- true
	}

	if passthrough {
		return nil
	}

	receiveStdoutError, stdinDone := setupStdioChannels(params.Streams, conn, detachKeys)
	if params.AttachReady != nil {
		params.AttachReady <- true
	}
	return readStdio(conn, params.Streams, receiveStdoutError, stdinDone)
}

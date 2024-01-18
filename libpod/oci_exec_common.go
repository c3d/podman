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

//go:build !remote && (linux || freebsd)

package libpod

import(
	"context"
	"io"
	"os"

	taskAPI "github.com/containerd/containerd/v2/api/runtime/task/v2"
	apitypes "github.com/containerd/containerd/v2/api/types"
	"github.com/containerd/containerd/v2/core/runtime/v2/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/plugins"
	ptypes "github.com/containerd/containerd/v2/protobuf/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
)


// ShimV2Task is a shimv2 runtime process
type ShimV2Task struct {
	PID		int	// PID of the shim process
	namespace	string	// Namespace for the container
	grpcAddress	string	// Address of libpod's grpc socket
	ttrpcAddress	string	// Address of libpod's ttrpc socket
	publishBinary	string	// binary path to publish events back
	containerID	string	// The ID of the container
}


func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.EventPlugin,
			plugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			pp, err := ic.GetByID(plugins.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}
			ss, err := ic.GetByID(plugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			return newTaskService(ic.Context, pp.(shim.Publisher), ss.(shutdown.Service))
		},
	})
}

type ManagerConfig struct {
	Root         string
	State        string
	Store        containers.Store
	Events       *exchange.Exchange
	Address      string
	TTRPCAddress string
	SchedCore    bool
	SandboxStore sandbox.Store
}

func NewShimManager(ctx context.Context, config *ManagerConfig) )(*shim.Manager {
	return manager{name: name}
}

type manager struct {
	name string
}

func (m manager) Name() string {
	return m.name
}

func (m manager) Start(ctx context.Context, id string, opts shim.StartOpts) (shim.BootstrapParams, error) {
	return shim.BootstrapParams{}, errdefs.ErrNotImplemented
}

func (m manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	return shim.StopStatus{}, errdefs.ErrNotImplemented
}

func (m manager) Info(ctx context.Context, optionsR io.Reader) (*apitypes.RuntimeInfo, error) {
	info := &apitypes.RuntimeInfo{
		Name: "io.containerd.podman.v1",
		Version: &apitypes.RuntimeVersion{
			Version: "v1.0.0",
		},
	}
	return info, nil
}

func newTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TaskService, error) {
	// The shim.Publisher and shutdown.Service are usually useful for your task service,
	// but we don't need them in the podmanTaskService.
	return &podmanTaskService{}, nil
}

var (
	_ = shim.TTRPCService(&podmanTaskService{})
)

type podmanTaskService struct {
}

// RegisterTTRPC allows TTRPC services to be registered with the underlying server
func (s *podmanTaskService) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTaskService(server, s)
	return nil
}

// Create a new container
func (s *podmanTaskService) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, err error) {
	return nil, errdefs.ErrNotImplemented
}

// Start the primary user process inside the container
func (s *podmanTaskService) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// Delete a process or container
func (s *podmanTaskService) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// Exec an additional process inside the container
func (s *podmanTaskService) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// ResizePty of a process
func (s *podmanTaskService) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// State returns runtime state of a process
func (s *podmanTaskService) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// Pause the container
func (s *podmanTaskService) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Resume the container
func (s *podmanTaskService) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Kill a process
func (s *podmanTaskService) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Pids returns all pids inside the container
func (s *podmanTaskService) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// CloseIO of a process
func (s *podmanTaskService) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Checkpoint the container
func (s *podmanTaskService) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Connect returns shim information of the underlying service
func (s *podmanTaskService) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// Shutdown is called after the underlying resources of the shim are cleaned up and the service can be stopped
func (s *podmanTaskService) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*ptypes.Empty, error) {
	os.Exit(0)
	return &ptypes.Empty{}, nil
}

// Stats returns container level system stats for a container and its processes
func (s *podmanTaskService) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

// Update the live container
func (s *podmanTaskService) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

// Wait for a process to exit
func (s *podmanTaskService) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

//go:build !remote && (linux || freebsd)

package libpod


// ShimV2Task is a shimv2 runtime process
type ShimV2Task struct {
	PID		int	// PID of the shim process
	namespace	string	// Namespace for the container
	grpcAddress	string	// Address of libpod's grpc socket
	ttrpcAddress	string	// Address of libpod's ttrpc socket
	publishBinary	string	// binary path to publish events back
	containerID	string	// The ID of the container
}

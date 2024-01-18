//go:build linux

package libpod

import (
	runcconfig "github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/devices"

	spec "github.com/opencontainers/runtime-spec/specs-go"
)

// GetLimits converts spec resource limits to cgroup consumable limits
func GetLimits(resource *spec.LinuxResources) (runcconfig.Resources, error) {
	if resource == nil {
		resource = &spec.LinuxResources{}
	}
	final := &runcconfig.Resources{}
	devs := []*devices.Rule{}

	// Devices
	for _, entry := range resource.Devices {
		if entry.Major == nil || entry.Minor == nil {
			continue
		}
		runeType := 'a'
		switch entry.Type {
		case "b":
			runeType = 'b'
		case "c":
			runeType = 'c'
		}

		devs = append(devs, &devices.Rule{
			Type:        devices.Type(runeType),
			Major:       *entry.Major,
			Minor:       *entry.Minor,
			Permissions: devices.Permissions(entry.Access),
			Allow:       entry.Allow,
		})
	}
	final.Devices = devs

	// HugepageLimits
	pageLimits := []*runcconfig.HugepageLimit{}
	for _, entry := range resource.HugepageLimits {
		pageLimits = append(pageLimits, &runcconfig.HugepageLimit{
			Pagesize: entry.Pagesize,
			Limit:    entry.Limit,
		})
	}
	final.HugetlbLimit = pageLimits

	// Networking
	netPriorities := []*runcconfig.IfPrioMap{}
	if resource.Network != nil {
		for _, entry := range resource.Network.Priorities {
			netPriorities = append(netPriorities, &runcconfig.IfPrioMap{
				Interface: entry.Name,
				Priority:  int64(entry.Priority),
			})
		}
	}
	final.NetPrioIfpriomap = netPriorities
	rdma := make(map[string]runcconfig.LinuxRdma)
	for name, entry := range resource.Rdma {
		rdma[name] = runcconfig.LinuxRdma{HcaHandles: entry.HcaHandles, HcaObjects: entry.HcaObjects}
	}
	final.Rdma = rdma

	// Memory
	if resource.Memory != nil {
		if resource.Memory.Limit != nil {
			final.Memory = *resource.Memory.Limit
		}
		if resource.Memory.Reservation != nil {
			final.MemoryReservation = *resource.Memory.Reservation
		}
		if resource.Memory.Swap != nil {
			final.MemorySwap = *resource.Memory.Swap
		}
		if resource.Memory.Swappiness != nil {
			final.MemorySwappiness = resource.Memory.Swappiness
		}
	}

	// CPU
	if resource.CPU != nil {
		if resource.CPU.Period != nil {
			final.CpuPeriod = *resource.CPU.Period
		}
		if resource.CPU.Quota != nil {
			final.CpuQuota = *resource.CPU.Quota
		}
		if resource.CPU.RealtimePeriod != nil {
			final.CpuRtPeriod = *resource.CPU.RealtimePeriod
		}
		if resource.CPU.RealtimeRuntime != nil {
			final.CpuRtRuntime = *resource.CPU.RealtimeRuntime
		}
		if resource.CPU.Shares != nil {
			final.CpuShares = *resource.CPU.Shares
		}
		final.CpusetCpus = resource.CPU.Cpus
		final.CpusetMems = resource.CPU.Mems
	}

	// BlkIO
	if resource.BlockIO != nil {
		if len(resource.BlockIO.ThrottleReadBpsDevice) > 0 {
			for _, entry := range resource.BlockIO.ThrottleReadBpsDevice {
				throttle := runcconfig.NewThrottleDevice(entry.Major, entry.Minor, entry.Rate)
				final.BlkioThrottleReadBpsDevice = append(final.BlkioThrottleReadBpsDevice, throttle)
			}
		}
		if len(resource.BlockIO.ThrottleWriteBpsDevice) > 0 {
			for _, entry := range resource.BlockIO.ThrottleWriteBpsDevice {
				throttle := runcconfig.NewThrottleDevice(entry.Major, entry.Minor, entry.Rate)
				final.BlkioThrottleWriteBpsDevice = append(final.BlkioThrottleWriteBpsDevice, throttle)
			}
		}
		if len(resource.BlockIO.ThrottleReadIOPSDevice) > 0 {
			for _, entry := range resource.BlockIO.ThrottleReadIOPSDevice {
				throttle := runcconfig.NewThrottleDevice(entry.Major, entry.Minor, entry.Rate)
				final.BlkioThrottleReadIOPSDevice = append(final.BlkioThrottleReadIOPSDevice, throttle)
			}
		}
		if len(resource.BlockIO.ThrottleWriteIOPSDevice) > 0 {
			for _, entry := range resource.BlockIO.ThrottleWriteIOPSDevice {
				throttle := runcconfig.NewThrottleDevice(entry.Major, entry.Minor, entry.Rate)
				final.BlkioThrottleWriteIOPSDevice = append(final.BlkioThrottleWriteIOPSDevice, throttle)
			}
		}
		if resource.BlockIO.LeafWeight != nil {
			final.BlkioLeafWeight = *resource.BlockIO.LeafWeight
		}
		if resource.BlockIO.Weight != nil {
			final.BlkioWeight = *resource.BlockIO.Weight
		}
		if len(resource.BlockIO.WeightDevice) > 0 {
			for _, entry := range resource.BlockIO.WeightDevice {
				var w, lw uint16
				if entry.Weight != nil {
					w = *entry.Weight
				}
				if entry.LeafWeight != nil {
					lw = *entry.LeafWeight
				}
				weight := runcconfig.NewWeightDevice(entry.Major, entry.Minor, w, lw)
				final.BlkioWeightDevice = append(final.BlkioWeightDevice, weight)
			}
		}
	}

	// Pids
	if resource.Pids != nil {
		final.PidsLimit = resource.Pids.Limit
	}

	// Networking
	if resource.Network != nil {
		if resource.Network.ClassID != nil {
			final.NetClsClassid = *resource.Network.ClassID
		}
	}

	// Unified state
	final.Unified = resource.Unified
	return *final, nil
}

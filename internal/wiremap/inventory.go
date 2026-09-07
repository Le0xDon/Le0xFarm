// Package wiremap converts trusted domain inventory into the protobuf wire model.
package wiremap

import (
	"github.com/le0xdon/le0xfarm/internal/model"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
)

func Inventory(in model.Inventory) *le0xv1.Inventory {
	out := &le0xv1.Inventory{
		HostId: in.Host.HostID.String(), Hostname: in.Host.Hostname,
		OsId: in.Host.OS.ID, OsVersion: in.Host.OS.VersionID,
		Architecture: in.Host.Architecture, CpuVendor: in.CPU.Vendor,
		CpuModel: in.CPU.Model, CpuCores: in.CPU.Cores, CpuThreads: in.CPU.Threads,
		MemoryTotalBytes: in.Memory.TotalBytes,
		Gpus:             make([]*le0xv1.GPU, 0, len(in.GPUs)),
	}
	for _, gpu := range in.GPUs {
		out.Gpus = append(out.Gpus, &le0xv1.GPU{DeviceId: gpu.DeviceID.String(), Vendor: gpu.Vendor, Model: gpu.Model, PciBusId: gpu.PCIBusID, Uuid: gpu.UUID})
	}
	return out
}

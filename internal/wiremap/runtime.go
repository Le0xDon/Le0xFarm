package wiremap

import (
	"maps"
	"slices"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/protocol"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func ExecutionPlan(plan model.ExecutionPlan) (*le0xv1.ExecutionPlan, error) {
	if err := plan.ExecutionID.Validate(); err != nil {
		return nil, typed(farmerr.CONFIG_CONFLICT, "invalid ExecutionID")
	}
	if err := plan.Ownership.Validate(); err != nil || plan.HostID != plan.Ownership.HostID || !slices.Equal(plan.DeviceIDs, plan.Ownership.DeviceIDs) {
		return nil, typed(farmerr.CONFIG_CONFLICT, "invalid ExecutionPlan ownership metadata")
	}
	out := &le0xv1.ExecutionPlan{ExecutionId: plan.ExecutionID.String(), Executable: plan.Executable, Args: append([]string(nil), plan.Args...), Environment: maps.Clone(plan.Environment), WorkingDirectory: plan.WorkingDirectory, RestartPolicy: string(plan.RestartPolicy), Ownership: WorkloadOwnership(plan.Ownership)}
	if spec := plan.Miner; spec != nil {
		if !slices.Equal(spec.GPUDeviceIDs, plan.Ownership.DeviceIDs) {
			return nil, typed(farmerr.CONFIG_CONFLICT, "miner DeviceIDs do not match ownership ResourceClaim")
		}
		miner := &le0xv1.MinerSpec{AdapterId: spec.AdapterID, SpecVersion: spec.SpecVersion, PackageId: spec.PackageID.String(), PackageVersion: spec.PackageVersion, Mode: string(spec.Mode), Coin: spec.Coin, Algorithm: spec.Algorithm,
			CpuThreads: cloneUint32(spec.CPUThreads), HugePages: cloneBool(spec.HugePages), Msr: cloneBool(spec.MSR), Options: maps.Clone(spec.Options)}
		for _, id := range spec.GPUDeviceIDs {
			miner.GpuDeviceIds = append(miner.GpuDeviceIds, id.String())
		}
		if endpoint := spec.Endpoint; endpoint != nil {
			miner.Endpoint = &le0xv1.MiningEndpoint{Address: endpoint.Address, Tls: endpoint.TLS, User: endpoint.User, Password: endpoint.Password, Worker: endpoint.Worker}
		}
		out.Miner = miner
	}
	return out, nil
}

func WorkloadOwnership(ownership model.WorkloadOwnership) *le0xv1.WorkloadOwnership {
	devices := make([]string, len(ownership.DeviceIDs))
	for i, id := range ownership.DeviceIDs {
		devices[i] = id.String()
	}
	return &le0xv1.WorkloadOwnership{WorkloadId: ownership.WorkloadID.String(), DesiredGeneration: ownership.DesiredGeneration, ResolvedHash: ownership.ResolvedHash, HostId: ownership.HostID.String(), ResourceClaim: &le0xv1.ResourceClaim{Cpu: ownership.CPU, DeviceIds: devices}}
}

func ParseWorkloadOwnership(in *le0xv1.WorkloadOwnership, expectedHost identity.HostID) (*model.WorkloadOwnership, error) {
	if in == nil {
		return nil, nil
	}
	if in.ResourceClaim == nil {
		return nil, typed(farmerr.CONFIG_CONFLICT, "ownership ResourceClaim is required")
	}
	workloadID, err := identity.ParseWorkloadID(in.WorkloadId)
	if err != nil {
		return nil, typed(farmerr.CONFIG_CONFLICT, "invalid ownership WorkloadID")
	}
	hostID, err := identity.ParseHostID(in.HostId)
	if err != nil {
		return nil, typed(farmerr.CONFIG_CONFLICT, "invalid ownership HostID")
	}
	devices := make([]identity.DeviceID, 0, len(in.ResourceClaim.DeviceIds))
	for _, raw := range in.ResourceClaim.DeviceIds {
		id, err := identity.ParseDeviceID(raw)
		if err != nil {
			return nil, typed(farmerr.CONFIG_CONFLICT, "invalid ownership DeviceID")
		}
		devices = append(devices, id)
	}
	result := &model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: in.DesiredGeneration, ResolvedHash: in.ResolvedHash, HostID: hostID, CPU: in.ResourceClaim.Cpu, DeviceIDs: devices}
	if err := result.Validate(); err != nil {
		return nil, typed(farmerr.CONFIG_CONFLICT, "invalid ownership metadata")
	}
	if expectedHost.Validate() == nil && result.HostID != expectedHost {
		return nil, typed(farmerr.CONFIG_CONFLICT, "ownership HostID does not match Agent session")
	}
	return result, nil
}

func ParseExecutions(in *le0xv1.Executions, expectedHost identity.HostID) ([]model.ExecutionObservation, error) {
	if in == nil {
		return nil, typed(farmerr.CONFIG_CONFLICT, "execution snapshot is required")
	}
	result := make([]model.ExecutionObservation, 0, len(in.Executions))
	seen := make(map[identity.ExecutionID]struct{}, len(in.Executions))
	for _, item := range in.Executions {
		observation, err := ParseExecution(item, expectedHost)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[observation.ExecutionID]; exists {
			return nil, typed(farmerr.CONFIG_CONFLICT, "duplicate ExecutionID in observation")
		}
		seen[observation.ExecutionID] = struct{}{}
		result = append(result, observation)
	}
	slices.SortFunc(result, func(a, b model.ExecutionObservation) int {
		return compare(a.ExecutionID.String(), b.ExecutionID.String())
	})
	return result, nil
}

func ParseExecution(in *le0xv1.Execution, expectedHost identity.HostID) (model.ExecutionObservation, error) {
	if in == nil {
		return model.ExecutionObservation{}, typed(farmerr.CONFIG_CONFLICT, "execution observation is required")
	}
	id, err := identity.ParseExecutionID(in.ExecutionId)
	if err != nil {
		return model.ExecutionObservation{}, typed(farmerr.CONFIG_CONFLICT, "invalid observed ExecutionID")
	}
	state := model.ExecutionStatus(in.State)
	switch state {
	case model.ExecutionStarting, model.ExecutionRunning, model.ExecutionStopping, model.ExecutionStopped, model.ExecutionCrashed, model.ExecutionBackoff, model.ExecutionFailed:
	default:
		return model.ExecutionObservation{}, typed(farmerr.CONFIG_CONFLICT, "invalid observed execution state")
	}
	ownership, err := ParseWorkloadOwnership(in.Ownership, expectedHost)
	if err != nil {
		return model.ExecutionObservation{}, err
	}
	result := model.ExecutionObservation{ExecutionID: id, Ownership: ownership, Status: state, PID: int(in.Pid), RestartCount: in.RestartCount, LastError: in.LastError}
	if in.StartedAt != nil {
		if err := in.StartedAt.CheckValid(); err != nil {
			return model.ExecutionObservation{}, typed(farmerr.CONFIG_CONFLICT, "invalid observed start timestamp")
		}
		result.StartedAt = in.StartedAt.AsTime().UTC()
	}
	if in.HasExitCode {
		value := int(in.ExitCode)
		result.ExitCode = &value
	}
	if telemetry := in.MinerTelemetry; telemetry != nil {
		parsed := &model.MinerTelemetry{AdapterID: telemetry.AdapterId, MinerVersion: telemetry.MinerVersion, Algorithm: telemetry.Algorithm,
			HashrateShortHPS: telemetry.HashrateShortHps, HashrateMediumHPS: telemetry.HashrateMediumHps, HashrateLongHPS: telemetry.HashrateLongHps, HighestHashrateHPS: telemetry.HighestHashrateHps,
			AcceptedShares: telemetry.AcceptedShares, RejectedShares: telemetry.RejectedShares, StaleShares: telemetry.StaleShares, TotalResults: telemetry.TotalResults,
			PoolConnected: telemetry.PoolConnected, PoolLatencyMS: telemetry.PoolLatencyMs, UptimeSeconds: telemetry.UptimeSeconds, HugePagesAvailable: telemetry.HugePagesAvailable,
			HugePagesPercent: telemetry.HugePagesPercent, MSRAvailable: telemetry.MsrAvailable, Age: time.Duration(telemetry.AgeMilliseconds) * time.Millisecond,
			Health: model.MinerHealth(telemetry.Health), ErrorCode: farmerr.Code(telemetry.ErrorCode), Message: telemetry.Message}
		if telemetry.CollectedAt != nil {
			if err := telemetry.CollectedAt.CheckValid(); err != nil {
				return model.ExecutionObservation{}, typed(farmerr.CONFIG_CONFLICT, "invalid miner telemetry timestamp")
			}
			parsed.CollectedAt = telemetry.CollectedAt.AsTime().UTC()
		}
		for _, device := range telemetry.PerDevice {
			if device == nil {
				return model.ExecutionObservation{}, typed(farmerr.CONFIG_CONFLICT, "invalid per-device telemetry")
			}
			deviceID, err := identity.ParseDeviceID(device.DeviceId)
			if err != nil {
				return model.ExecutionObservation{}, typed(farmerr.CONFIG_CONFLICT, "invalid telemetry DeviceID")
			}
			parsed.PerDevice = append(parsed.PerDevice, model.DeviceHashrate{DeviceID: deviceID, HashrateHPS: device.HashrateHps})
		}
		result.MinerTelemetry = parsed
	}
	return result, nil
}

func ParseInventory(in *le0xv1.Inventory, expectedHost identity.HostID) (model.Inventory, error) {
	if in == nil {
		return model.Inventory{}, typed(farmerr.CONFIG_CONFLICT, "inventory is required")
	}
	hostID, err := identity.ParseHostID(in.HostId)
	if err != nil || hostID != expectedHost {
		return model.Inventory{}, typed(farmerr.CONFIG_CONFLICT, "inventory HostID does not match Agent session")
	}
	result := model.Inventory{SchemaVersion: protocol.CurrentSchemaVersion, Host: model.Host{HostID: hostID, Hostname: in.Hostname, OS: model.OSInfo{ID: in.OsId, VersionID: in.OsVersion}, Architecture: in.Architecture}, CPU: model.CPU{Vendor: in.CpuVendor, Model: in.CpuModel, Cores: in.CpuCores, Threads: in.CpuThreads}, Memory: model.Memory{TotalBytes: in.MemoryTotalBytes}}
	seen := make(map[identity.DeviceID]struct{}, len(in.Gpus))
	for _, gpu := range in.Gpus {
		if gpu == nil {
			return model.Inventory{}, typed(farmerr.CONFIG_CONFLICT, "invalid GPU inventory item")
		}
		deviceID, err := identity.ParseDeviceID(gpu.DeviceId)
		if err != nil {
			return model.Inventory{}, typed(farmerr.CONFIG_CONFLICT, "invalid inventory DeviceID")
		}
		if _, exists := seen[deviceID]; exists {
			return model.Inventory{}, typed(farmerr.CONFIG_CONFLICT, "duplicate inventory DeviceID")
		}
		seen[deviceID] = struct{}{}
		result.GPUs = append(result.GPUs, model.GPU{DeviceID: deviceID, Vendor: gpu.Vendor, Model: gpu.Model, PCIBusID: gpu.PciBusId, UUID: gpu.Uuid})
	}
	return result, nil
}

func Timestamp(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() {
		return nil
	}
	return timestamppb.New(value)
}

func cloneUint32(value *uint32) *uint32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func compare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func typed(code farmerr.Code, message string) error {
	return farmerr.Error{Code: code, HumanMessage: message}
}

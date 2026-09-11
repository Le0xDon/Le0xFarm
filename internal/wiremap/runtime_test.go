package wiremap

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/protobuf/proto"
)

func TestUnmanagedProcessWireRoundTripAndStrictValidation(t *testing.T) {
	device, _ := identity.ParseDeviceID("device_0123456789abcdef0123456789abcdef")
	input := []model.UnmanagedProcessObservation{{PID: 42, Executable: "gpu-miner", ProcessInstance: "linux-proc-start-ticks:99", GPURelevant: true, ResourceScope: model.ProcessResourcesExact, DeviceIDs: []identity.DeviceID{device}, Evidence: []model.ProcessEvidence{{Kind: "KNOWN_ADAPTER_EXECUTABLE", Provider: "test", Detail: "registered basename"}}, ObservedAt: time.Unix(123, 0).UTC()}}
	got, err := ParseUnmanagedProcesses(UnmanagedProcesses(input))
	if err != nil || len(got) != 1 || got[0].PID != 42 || got[0].DeviceIDs[0] != device || got[0].Evidence[0].Provider != "test" {
		t.Fatalf("round trip=%+v err=%v", got, err)
	}
	bad := UnmanagedProcesses(input)
	bad.Processes[0].Executable = "/tmp/gpu-miner"
	if _, err := ParseUnmanagedProcesses(bad); err == nil {
		t.Fatal("process path was accepted instead of a non-secret basename")
	}
	bad = UnmanagedProcesses(input)
	bad.Processes[0].Evidence = nil
	if _, err := ParseUnmanagedProcesses(bad); err == nil {
		t.Fatal("unproven process classification was accepted")
	}
}

func TestUsefulWorkEvidenceWireValidationPreservesZeroAndUnknown(t *testing.T) {
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	zero := 0.0
	accepted := uint64(0)
	collected := time.Unix(1234, 0).UTC()
	wire := &le0xv1.Execution{ExecutionId: "execution_0123456789abcdef0123456789abcdef", State: "RUNNING", Pid: 42, StartedAt: Timestamp(collected.Add(-time.Second)), MinerTelemetry: &le0xv1.MinerTelemetry{Health: "DEGRADED", CollectedAt: Timestamp(collected)}, UsefulWork: &le0xv1.UsefulWorkEvidence{Provider: "future-compute-adapter", Availability: "AVAILABLE", UsefulWork: "NOT_CONFIRMED", AcceptedWork: &accepted, Upstream: "UNKNOWN", Confidence: "ADAPTER_REPORTED", CollectedAt: Timestamp(collected), FreshForMilliseconds: 10_000, Metrics: []*le0xv1.WorkMetric{{Kind: "TASKS_PER_SECOND", Unit: "TASK/S", Value: zero}}}}
	parsed, err := ParseExecution(wire, host)
	if err != nil || parsed.MinerTelemetry == nil || parsed.UsefulWork == nil {
		t.Fatalf("evidence parse=%+v err=%v", parsed, err)
	}
	evidence := parsed.UsefulWork
	if evidence.AcceptedWork == nil || *evidence.AcceptedWork != 0 || len(evidence.Metrics) != 1 || evidence.Metrics[0].Value != 0 || evidence.Upstream != model.UpstreamUnknown {
		t.Fatalf("explicit zero was confused with unknown: %+v", evidence)
	}
	bad := proto.Clone(wire).(*le0xv1.Execution)
	bad.UsefulWork.Metrics[0].Value = math.Inf(1)
	if _, err := ParseExecution(bad, host); err == nil {
		t.Fatal("non-finite metric accepted")
	}
	bad = proto.Clone(wire).(*le0xv1.Execution)
	bad.UsefulWork.Availability = "FRESH_ENOUGH_MAYBE"
	if _, err := ParseExecution(bad, host); err == nil {
		t.Fatal("unknown availability accepted")
	}
}

func TestExecutionPlanWireOwnershipAndResolvedDataOnly(t *testing.T) {
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	workload, _ := identity.ParseWorkloadID("workload_0123456789abcdef0123456789abcdef")
	execution, _ := identity.ParseExecutionID("execution_0123456789abcdef0123456789abcdef")
	packageID, _ := identity.ParsePackageID("package_0123456789abcdef0123456789abcdef")
	walletID, _ := identity.ParseWalletID("wallet_0123456789abcdef0123456789abcdef")
	poolID, _ := identity.ParsePoolID("pool_0123456789abcdef0123456789abcdef")
	owner := model.WorkloadOwnership{WorkloadID: workload, DesiredGeneration: 4, ResolvedHash: "sha256:" + strings.Repeat("a", 64), HostID: host, CPU: true}
	plan := model.ExecutionPlan{ExecutionID: execution, Ownership: owner, HostID: host, Miner: &model.MinerSpec{AdapterID: "test", SpecVersion: 1, PackageID: packageID, PackageVersion: "1", WalletID: &walletID, PoolID: &poolID, Mode: model.MinerModeMining, Endpoint: &model.MiningEndpoint{Address: "pool.example:443", User: "public-login"}}}
	wire, err := ExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if wire.GetOwnership().GetDesiredGeneration() != 4 || !wire.GetOwnership().GetResourceClaim().GetCpu() || wire.GetMiner().GetEndpoint().GetUser() != "public-login" {
		t.Fatalf("resolved plan metadata lost: %+v", wire)
	}
	fields := wire.GetMiner().ProtoReflect().Descriptor().Fields()
	if fields.ByName("wallet_id") != nil || fields.ByName("pool_id") != nil {
		t.Fatal("Controller database provenance leaked into Agent wire contract")
	}
}

func TestExecutionObservationValidationAndExplicitUnmanaged(t *testing.T) {
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	base := &le0xv1.Execution{ExecutionId: "execution_0123456789abcdef0123456789abcdef", State: "RUNNING", Pid: 42, StartedAt: Timestamp(time.Unix(100, 0).UTC()), Ownership: &le0xv1.WorkloadOwnership{WorkloadId: "workload_0123456789abcdef0123456789abcdef", DesiredGeneration: 1, ResolvedHash: "sha256:" + strings.Repeat("a", 64), HostId: host.String(), ResourceClaim: &le0xv1.ResourceClaim{Cpu: true}}}
	parsed, err := ParseExecution(base, host)
	if err != nil || parsed.Ownership == nil || parsed.Ownership.WorkloadID.String() != base.Ownership.WorkloadId {
		t.Fatalf("valid ownership rejected: %+v err=%v", parsed, err)
	}
	unmanaged := &le0xv1.Execution{ExecutionId: "execution_1123456789abcdef0123456789abcdef", State: "RUNNING", Pid: 43, StartedAt: Timestamp(time.Unix(101, 0).UTC())}
	parsed, err = ParseExecution(unmanaged, host)
	if err != nil || parsed.Ownership != nil {
		t.Fatalf("unmanaged execution not represented explicitly: %+v err=%v", parsed, err)
	}
	bad := proto.Clone(base).(*le0xv1.Execution)
	bad.Ownership = &le0xv1.WorkloadOwnership{WorkloadId: base.Ownership.WorkloadId, DesiredGeneration: 0, ResolvedHash: base.Ownership.ResolvedHash, HostId: host.String(), ResourceClaim: &le0xv1.ResourceClaim{Cpu: true}}
	if _, err := ParseExecution(bad, host); err == nil {
		t.Fatal("malformed present ownership accepted")
	}
	if _, err := ParseExecutions(&le0xv1.Executions{Executions: []*le0xv1.Execution{base, base}}, host); err == nil {
		t.Fatal("duplicate execution observation accepted")
	}
}

func TestExecutionObservationRejectsImpossibleActiveUsefulWork(t *testing.T) {
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	collected := time.Unix(1_234, 0).UTC()
	base := &le0xv1.Execution{
		ExecutionId:    "execution_0123456789abcdef0123456789abcdef",
		State:          "RUNNING",
		Pid:            42,
		StartedAt:      Timestamp(collected.Add(-time.Second)),
		Ownership:      &le0xv1.WorkloadOwnership{WorkloadId: "workload_0123456789abcdef0123456789abcdef", DesiredGeneration: 1, ResolvedHash: "sha256:" + strings.Repeat("a", 64), HostId: host.String(), ResourceClaim: &le0xv1.ResourceClaim{Cpu: true}},
		MinerTelemetry: &le0xv1.MinerTelemetry{AdapterId: "test", Health: "MINING", CollectedAt: Timestamp(collected)},
		UsefulWork:     &le0xv1.UsefulWorkEvidence{Provider: "generic-test", Availability: "AVAILABLE", UsefulWork: "CONFIRMED", Upstream: "UNKNOWN", Confidence: "ADAPTER_REPORTED", CollectedAt: Timestamp(collected), FreshForMilliseconds: 10_000},
	}
	if _, err := ParseExecution(base, host); err != nil {
		t.Fatalf("valid running confirmed observation rejected: %v", err)
	}
	stopped := proto.Clone(base).(*le0xv1.Execution)
	stopped.State, stopped.Pid, stopped.StartedAt = "STOPPED", 0, nil
	if _, err := ParseExecution(stopped, host); err == nil {
		t.Fatal("STOPPED PID 0 observation authorized MINING")
	}
	missingPID := proto.Clone(base).(*le0xv1.Execution)
	missingPID.Pid = 0
	if _, err := ParseExecution(missingPID, host); err == nil {
		t.Fatal("RUNNING PID 0 observation authorized MINING")
	}
	missingStart := proto.Clone(base).(*le0xv1.Execution)
	missingStart.StartedAt = nil
	if _, err := ParseExecution(missingStart, host); err == nil {
		t.Fatal("RUNNING observation without process start identity authorized MINING")
	}
	unmanaged := proto.Clone(base).(*le0xv1.Execution)
	unmanaged.Ownership = nil
	if _, err := ParseExecution(unmanaged, host); err == nil {
		t.Fatal("unmanaged observation authorized confirmed useful work")
	}

	historical := proto.Clone(base).(*le0xv1.Execution)
	historical.State, historical.Pid = "STOPPED", 0
	historical.MinerTelemetry.Health = "DEGRADED"
	historical.UsefulWork.Availability = "STALE"
	historical.UsefulWork.ReasonCode = "TELEMETRY_STALE"
	if parsed, err := ParseExecution(historical, host); err != nil || parsed.Status != model.ExecutionStopped || parsed.UsefulWork.Availability != model.TelemetryStale {
		t.Fatalf("safe historical stopped evidence rejected: %+v err=%v", parsed, err)
	}
	starting := &le0xv1.Execution{ExecutionId: base.ExecutionId, State: "STARTING", Ownership: base.Ownership, MinerTelemetry: &le0xv1.MinerTelemetry{AdapterId: "test", Health: "STARTING"}, UsefulWork: &le0xv1.UsefulWorkEvidence{Provider: "generic-test", Availability: "UNAVAILABLE", UsefulWork: "UNKNOWN", Upstream: "UNKNOWN", Confidence: "UNKNOWN", ReasonCode: "TELEMETRY_UNAVAILABLE"}}
	if _, err := ParseExecution(starting, host); err != nil {
		t.Fatalf("legitimate STARTING observation rejected: %v", err)
	}
}

func TestInventoryRequiresUniqueHostDevices(t *testing.T) {
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	device := "device_0123456789abcdef0123456789abcdef"
	if _, err := ParseInventory(&le0xv1.Inventory{HostId: host.String(), Gpus: []*le0xv1.GPU{{DeviceId: device}, {DeviceId: device}}}, host); err == nil {
		t.Fatal("duplicate inventory DeviceID accepted")
	}
}

func TestExecutionPlanCarriesExactGPUAssignmentAndRejectsMalformedBinding(t *testing.T) {
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	workload, _ := identity.ParseWorkloadID("workload_0123456789abcdef0123456789abcdef")
	execution, _ := identity.ParseExecutionID("execution_0123456789abcdef0123456789abcdef")
	packageID, _ := identity.ParsePackageID("package_0123456789abcdef0123456789abcdef")
	device, _ := identity.ParseDeviceID("device_0123456789abcdef0123456789abcdef")
	assignment := model.GPUAssignment{DeviceID: device, HardwareIdentity: "gpu-stable", RuntimeSelector: "01:00.0"}
	owner := model.WorkloadOwnership{WorkloadID: workload, DesiredGeneration: 1, ResolvedHash: "sha256:" + strings.Repeat("b", 64), HostID: host, DeviceIDs: []identity.DeviceID{device}}
	plan := model.ExecutionPlan{ExecutionID: execution, Ownership: owner, HostID: host, DeviceIDs: []identity.DeviceID{device}, Miner: &model.MinerSpec{AdapterID: "gpu-test", SpecVersion: 1, PackageID: packageID, PackageVersion: "1", Mode: model.MinerModeMining, GPUDeviceIDs: []identity.DeviceID{device}, GPUAssignments: []model.GPUAssignment{assignment}}}
	wire, err := ExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	got := wire.GetMiner().GetGpuAssignments()
	if len(got) != 1 || got[0].GetDeviceId() != device.String() || got[0].GetHardwareIdentity() != "gpu-stable" || got[0].GetRuntimeSelector() != "01:00.0" {
		t.Fatalf("GPU binding lost: %+v", got)
	}
	plan.Miner.GPUAssignments[0].RuntimeSelector = "GPU0"
	if _, err := ExecutionPlan(plan); err == nil {
		t.Fatal("ordinal-only GPU selector accepted")
	}
}

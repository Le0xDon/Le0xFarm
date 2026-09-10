package wiremap

import (
	"strings"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/protobuf/proto"
)

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
	base := &le0xv1.Execution{ExecutionId: "execution_0123456789abcdef0123456789abcdef", State: "RUNNING", Ownership: &le0xv1.WorkloadOwnership{WorkloadId: "workload_0123456789abcdef0123456789abcdef", DesiredGeneration: 1, ResolvedHash: "sha256:" + strings.Repeat("a", 64), HostId: host.String(), ResourceClaim: &le0xv1.ResourceClaim{Cpu: true}}}
	parsed, err := ParseExecution(base, host)
	if err != nil || parsed.Ownership == nil || parsed.Ownership.WorkloadID.String() != base.Ownership.WorkloadId {
		t.Fatalf("valid ownership rejected: %+v err=%v", parsed, err)
	}
	unmanaged := &le0xv1.Execution{ExecutionId: "execution_1123456789abcdef0123456789abcdef", State: "RUNNING"}
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

func TestInventoryRequiresUniqueHostDevices(t *testing.T) {
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	device := "device_0123456789abcdef0123456789abcdef"
	if _, err := ParseInventory(&le0xv1.Inventory{HostId: host.String(), Gpus: []*le0xv1.GPU{{DeviceId: device}, {DeviceId: device}}}, host); err == nil {
		t.Fatal("duplicate inventory DeviceID accepted")
	}
}

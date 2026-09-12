package processobserve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/reconcile"
)

func TestObserveConservativeClassificationManagedExclusionAndPIDReuse(t *testing.T) {
	root := t.TempDir()
	deviceID, _ := identity.ParseDeviceID("device_0123456789abcdef0123456789abcdef")
	makeProcess(t, root, 101, "xmrig", 700, true, "0000:01:00.0")
	makeProcess(t, root, 102, "ordinary", 701, false, "")
	makeProcess(t, root, 103, "xmrig", 702, false, "")

	source := Source{Root: root, Now: func() time.Time { return time.Unix(123, 0).UTC() }}
	signatures := []model.ProcessSignature{{Executable: "xmrig", Provider: "xmrig", CPURelevant: true, GPURelevant: true}}
	inventory := model.Inventory{GPUs: []model.GPU{{DeviceID: deviceID, PCIBusID: "0000:01:00.0"}}}
	got, err := source.Observe(inventory, signatures, []ManagedInstance{{PID: 101, ProcessInstance: "linux-proc-start-ticks:700"}, {PID: 103, ProcessInstance: "linux-proc-start-ticks:999"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PID != 103 || got[0].ProcessInstance != "linux-proc-start-ticks:702" {
		t.Fatalf("observations=%+v", got)
	}
	if got[0].ResourceScope != model.ProcessResourcesUnknown || !got[0].CPURelevant || !got[0].GPURelevant {
		t.Fatalf("PID-reused process attribution=%+v", got[0])
	}
	if strings.Contains(fmt.Sprintf("%+v", got), "ordinary") {
		t.Fatal("ordinary process was classified merely because it exists")
	}
}

func TestObserveExactGPUWithoutReadingSensitiveArgvOrEnvironment(t *testing.T) {
	root := t.TempDir()
	deviceID, _ := identity.ParseDeviceID("device_0123456789abcdef0123456789abcdef")
	makeProcess(t, root, 201, "gpu-miner", 800, true, "0000:01:00.0")
	secret := "wallet-seed-super-secret"
	if err := os.WriteFile(filepath.Join(root, "proc", "201", "cmdline"), []byte("gpu-miner\x00--token="+secret), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "201", "environ"), []byte("PASSWORD="+secret), 0600); err != nil {
		t.Fatal(err)
	}
	source := Source{Root: root}
	got, err := source.Observe(model.Inventory{GPUs: []model.GPU{{DeviceID: deviceID, PCIBusID: "0000:01:00.0"}}}, []model.ProcessSignature{{Executable: "gpu-miner", Provider: "test-gpu", GPURelevant: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ResourceScope != model.ProcessResourcesExact || len(got[0].DeviceIDs) != 1 || got[0].DeviceIDs[0] != deviceID {
		t.Fatalf("exact GPU observation=%+v", got)
	}
	if strings.Contains(fmt.Sprintf("%+v", got), secret) {
		t.Fatal("sensitive argv/environment material was surfaced")
	}
}

func TestAgentRestartDoesNotAdoptSurvivingUnprovenProcess(t *testing.T) {
	root := t.TempDir()
	makeProcess(t, root, 301, "xmrig", 900, false, "")
	source := Source{Root: root}
	signatures := []model.ProcessSignature{{Executable: "xmrig", Provider: "xmrig", CPURelevant: true}}
	managed := []ManagedInstance{{PID: 301, ProcessInstance: "linux-proc-start-ticks:900"}}
	before, err := source.Observe(model.Inventory{}, signatures, managed)
	if err != nil || len(before) != 0 {
		t.Fatalf("currently owned process classified as unmanaged: %+v %v", before, err)
	}
	// A reconstructed Agent/Supervisor has no in-memory proof tying the old
	// process to its current execution. It observes rather than adopts or kills.
	after, err := source.Observe(model.Inventory{}, signatures, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].PID != 301 || after[0].ResourceScope != model.ProcessResourcesExact || !after[0].CPU {
		t.Fatalf("surviving unproven process was not conservatively observed: %+v", after)
	}
}

func TestObservePermissionDeniedExecutableUsesExactRegisteredComm(t *testing.T) {
	for _, permission := range []error{syscall.EACCES, syscall.EPERM} {
		permission := permission
		t.Run(permission.Error(), func(t *testing.T) {
			root := t.TempDir()
			makeProcess(t, root, 401, "xmrig", 1001, false, "")
			source := sourceWithExeError(root, permission)
			got, err := source.Observe(model.Inventory{}, []model.ProcessSignature{{Executable: "xmrig", Provider: "xmrig", CPURelevant: true}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].PID != 401 || !got[0].CPURelevant || !got[0].CPU || got[0].ResourceScope != model.ProcessResourcesExact {
				t.Fatalf("permission-denied candidate=%+v", got)
			}
			if len(got[0].Evidence) != 1 || got[0].Evidence[0].Kind != "REGISTERED_COMM_IDENTITY_UNREADABLE" {
				t.Fatalf("candidate evidence=%+v", got[0].Evidence)
			}
		})
	}
}

func TestObserveUnreadableExecutableDoesNotFuzzilyClassifyUnrelatedComm(t *testing.T) {
	root := t.TempDir()
	makeProcess(t, root, 402, "not-xmrig", 1002, false, "")
	source := sourceWithExeError(root, syscall.EACCES)
	got, err := source.Observe(model.Inventory{}, []model.ProcessSignature{{Executable: "xmrig", Provider: "xmrig", CPURelevant: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unrelated unreadable process classified as miner: %+v", got)
	}
}

func TestObserveUnreadableGPUCandidateUsesConservativeUnknownScope(t *testing.T) {
	root := t.TempDir()
	makeProcess(t, root, 406, "gpu-miner", 1006, false, "")
	source := sourceWithExeError(root, syscall.EPERM)
	got, err := source.Observe(model.Inventory{}, []model.ProcessSignature{{Executable: "gpu-miner", Provider: "test-gpu", GPURelevant: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].GPURelevant || got[0].ResourceScope != model.ProcessResourcesUnknown || len(got[0].DeviceIDs) != 0 {
		t.Fatalf("unreadable GPU candidate was not conservatively broad: %+v", got)
	}
}

func TestObserveExecutableENOENTIsHarmlessProcessExitRace(t *testing.T) {
	root := t.TempDir()
	makeProcess(t, root, 403, "xmrig", 1003, false, "")
	source := sourceWithExeError(root, syscall.ENOENT)
	got, err := source.Observe(model.Inventory{}, []model.ProcessSignature{{Executable: "xmrig", Provider: "xmrig", CPURelevant: true}}, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("exited process observation=%+v err=%v", got, err)
	}
}

func TestObserveUnexpectedExecutableErrorFailsClosed(t *testing.T) {
	root := t.TempDir()
	makeProcess(t, root, 407, "xmrig", 1007, false, "")
	source := sourceWithExeError(root, syscall.EIO)
	if got, err := source.Observe(model.Inventory{}, []model.ProcessSignature{{Executable: "xmrig", Provider: "xmrig", CPURelevant: true}}, nil); err == nil || len(got) != 0 {
		t.Fatalf("unexpected procfs error did not fail closed: observations=%+v err=%v", got, err)
	}
}

func TestObserveUnreadableExecutablePreservesManagedExclusionAndPIDReuse(t *testing.T) {
	root := t.TempDir()
	makeProcess(t, root, 404, "xmrig", 1004, false, "")
	source := sourceWithExeError(root, syscall.EACCES)
	signatures := []model.ProcessSignature{{Executable: "xmrig", Provider: "xmrig", CPURelevant: true}}
	got, err := source.Observe(model.Inventory{}, signatures, []ManagedInstance{{PID: 404, ProcessInstance: "linux-proc-start-ticks:1004"}})
	if err != nil || len(got) != 0 {
		t.Fatalf("exact managed process classified as unmanaged: %+v err=%v", got, err)
	}
	got, err = source.Observe(model.Inventory{}, signatures, []ManagedInstance{{PID: 404, ProcessInstance: "linux-proc-start-ticks:999"}})
	if err != nil || len(got) != 1 || got[0].ProcessInstance != "linux-proc-start-ticks:1004" {
		t.Fatalf("PID-reused candidate was excluded: %+v err=%v", got, err)
	}
}

func TestUnreadableRegisteredCandidateBlocksThenFreshAbsenceAllowsStart(t *testing.T) {
	root := t.TempDir()
	makeProcess(t, root, 405, "xmrig", 1005, false, "")
	source := sourceWithExeError(root, syscall.EACCES)
	signatures := []model.ProcessSignature{{Executable: "xmrig", Provider: "xmrig", CPURelevant: true}}
	processes, err := source.Observe(model.Inventory{}, signatures, nil)
	if err != nil {
		t.Fatal(err)
	}
	host, _ := identity.NewHostID()
	workloadID, _ := identity.NewWorkloadID()
	executionID, _ := identity.NewExecutionID()
	claim := farmmodel.ResourceClaim{CPU: true}
	workload := farmmodel.DesiredWorkload{WorkloadID: workloadID, DesiredGeneration: 1, DesiredWorkloadContent: farmmodel.DesiredWorkloadContent{HostID: host, RunState: farmmodel.DesiredRunning, Resources: claim}}
	snapshot := farmmodel.ResolvedExecutionSnapshot{WorkloadID: workloadID, DesiredGeneration: 1, ExecutionID: executionID, HostID: host, Resources: claim}
	observed := controllerstate.HostObservation{HostID: host, Connected: true, Ready: true, Fresh: true, InventoryFresh: true, ProcessesFresh: true, UnmanagedProcesses: processes}
	input := reconcile.PlanInput{Workload: workload, Current: &snapshot, Snapshots: []farmmodel.ResolvedExecutionSnapshot{snapshot}, Observed: observed}
	blocked := reconcile.Plan(input)
	if blocked.Code != farmerr.UNMANAGED_PROCESS_CONFLICT || blocked.Conflict == "" || len(blocked.Actions) != 0 {
		t.Fatalf("unreadable candidate did not block START: %+v", blocked)
	}
	if err := os.RemoveAll(filepath.Join(root, "proc", "405")); err != nil {
		t.Fatal(err)
	}
	processes, err = source.Observe(model.Inventory{}, signatures, nil)
	if err != nil || len(processes) != 0 {
		t.Fatalf("fresh absence=%+v err=%v", processes, err)
	}
	input.Observed.UnmanagedProcesses = processes
	allowed := reconcile.Plan(input)
	if len(allowed.Actions) != 1 || allowed.Actions[0].Key.Action != reconcile.ActionStart {
		t.Fatalf("fresh absence did not allow START: %+v", allowed)
	}
}

func sourceWithExeError(root string, cause error) Source {
	return Source{Root: root, readlink: func(path string) (string, error) {
		if filepath.Base(path) == "exe" {
			return "", &os.PathError{Op: "readlink", Path: path, Err: cause}
		}
		return os.Readlink(path)
	}}
}

func makeProcess(t *testing.T, root string, pid int, executable string, startTicks uint64, gpu bool, pci string) {
	t.Helper()
	procDir := filepath.Join(root, "proc", fmt.Sprint(pid))
	if err := os.MkdirAll(filepath.Join(procDir, "fd"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("opt", executable), filepath.Join(procDir, "exe")); err != nil {
		t.Fatal(err)
	}
	fields := []string{"S"}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, fmt.Sprint(startTicks))
	if err := os.WriteFile(filepath.Join(procDir, "stat"), []byte(fmt.Sprintf("%d (%s) %s", pid, executable, strings.Join(fields, " "))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "comm"), []byte(executable+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if !gpu {
		return
	}
	if err := os.Symlink("/dev/dri/renderD128", filepath.Join(procDir, "fd", "9")); err != nil {
		t.Fatal(err)
	}
	drm := filepath.Join(root, "sys", "class", "drm", "renderD128")
	if err := os.MkdirAll(drm, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "..", "devices", pci), filepath.Join(drm, "device")); err != nil {
		t.Fatal(err)
	}
}

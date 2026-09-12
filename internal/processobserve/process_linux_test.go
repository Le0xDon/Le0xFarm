package processobserve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
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

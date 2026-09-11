package gpuresource

import (
	"fmt"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

func TestResolveUsesStableIdentityAndCanonicalOrder(t *testing.T) {
	first, second := testDevice(1), testDevice(2)
	inventory := model.Inventory{GPUs: []model.GPU{
		{DeviceID: second, UUID: " GPU-B ", PCIBusID: "0000:02:00.0", Vendor: "NVIDIA", Model: " RTX 5090 "},
		{DeviceID: first, UUID: "GPU-A", PCIBusID: "01:00.0", Vendor: "nvidia", Model: "RTX 4080"},
	}}
	bindings, err := Resolve([]identity.DeviceID{second, first}, inventory, []string{"NVIDIA"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 || bindings[0].DeviceID != first || bindings[1].DeviceID != second {
		t.Fatalf("bindings not canonical: %+v", bindings)
	}
	if bindings[0].HardwareIdentity != "gpu-a" || bindings[0].RuntimeSelector != "01:00.0" {
		t.Fatalf("binding not normalized: %+v", bindings[0])
	}

	reordered := inventory
	reordered.GPUs = []model.GPU{inventory.GPUs[1], inventory.GPUs[0]}
	again, err := Resolve([]identity.DeviceID{first, second}, reordered, []string{"nvidia"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrent([]identity.DeviceID{first, second}, bindings, reordered, []string{"nvidia"}); err != nil {
		t.Fatalf("reordered inventory changed identity: %v", err)
	}
	if len(again) != len(bindings) {
		t.Fatal("binding count changed")
	}
	for i := range again {
		if again[i] != bindings[i] {
			t.Fatalf("binding changed at %d: %+v != %+v", i, again[i], bindings[i])
		}
	}
}

func TestResolveFailsClosedWithoutExactUnambiguousGPU(t *testing.T) {
	device := testDevice(1)
	tests := []struct {
		name      string
		requested []identity.DeviceID
		gpus      []model.GPU
		vendors   []string
	}{
		{"missing", []identity.DeviceID{device}, nil, nil},
		{"no stable identity", []identity.DeviceID{device}, []model.GPU{{DeviceID: device, PCIBusID: "01:00.0"}}, nil},
		{"placeholder identity", []identity.DeviceID{device}, []model.GPU{{DeviceID: device, UUID: "0000000000000000", PCIBusID: "01:00.0"}}, nil},
		{"invalid selector", []identity.DeviceID{device}, []model.GPU{{DeviceID: device, UUID: "gpu-a", PCIBusID: "GPU0"}}, nil},
		{"unsupported vendor", []identity.DeviceID{device}, []model.GPU{{DeviceID: device, UUID: "gpu-a", PCIBusID: "01:00.0", Vendor: "amd"}}, []string{"nvidia"}},
		{"duplicate request", []identity.DeviceID{device, device}, []model.GPU{{DeviceID: device, UUID: "gpu-a", PCIBusID: "01:00.0"}}, nil},
		{"duplicate identity", []identity.DeviceID{device}, []model.GPU{{DeviceID: device, UUID: "gpu-a", PCIBusID: "01:00.0"}, {DeviceID: testDevice(2), UUID: "GPU-A", PCIBusID: "02:00.0"}}, nil},
		{"duplicate selector", []identity.DeviceID{device}, []model.GPU{{DeviceID: device, UUID: "gpu-a", PCIBusID: "01:00.0"}, {DeviceID: testDevice(2), UUID: "gpu-b", PCIBusID: "01:00.0"}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(tc.requested, model.Inventory{GPUs: tc.gpus}, tc.vendors)
			if code, ok := farmerr.CodeOf(err); !ok || code != farmerr.INCOMPATIBLE_HARDWARE {
				t.Fatalf("code=%s err=%v", code, err)
			}
		})
	}
}

func TestValidateCurrentRejectsReplacementAndSelectorChange(t *testing.T) {
	device := testDevice(1)
	original := model.Inventory{GPUs: []model.GPU{{DeviceID: device, UUID: "gpu-a", PCIBusID: "01:00.0", Vendor: "nvidia"}}}
	bindings, err := Resolve([]identity.DeviceID{device}, original, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []model.GPU{
		{DeviceID: device, UUID: "gpu-replacement", PCIBusID: "01:00.0", Vendor: "nvidia"},
		{DeviceID: device, UUID: "gpu-a", PCIBusID: "02:00.0", Vendor: "nvidia"},
	} {
		if err := ValidateCurrent([]identity.DeviceID{device}, bindings, model.Inventory{GPUs: []model.GPU{changed}}, nil); err == nil {
			t.Fatalf("changed hardware accepted: %+v", changed)
		}
	}
	if err := ValidateStableIdentities([]identity.DeviceID{device}, bindings, model.Inventory{GPUs: []model.GPU{{DeviceID: device, UUID: "gpu-a", PCIBusID: "02:00.0", Vendor: "nvidia"}}}, nil); err != nil {
		t.Fatalf("same physical GPU moved slots was rejected: %v", err)
	}
	if err := ValidateStableIdentities([]identity.DeviceID{device}, bindings, model.Inventory{GPUs: []model.GPU{{DeviceID: device, UUID: "replacement", PCIBusID: "01:00.0", Vendor: "nvidia"}}}, nil); err == nil {
		t.Fatal("different physical identity inherited DeviceID")
	}
}

func testDevice(value int) identity.DeviceID {
	id, err := identity.ParseDeviceID(fmt.Sprintf("device_%032x", value))
	if err != nil {
		panic(err)
	}
	return id
}

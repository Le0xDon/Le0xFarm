package model_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

func TestWorkloadOwnershipValidation(t *testing.T) {
	workloadID, _ := identity.ParseWorkloadID("workload_0123456789abcdef0123456789abcdef")
	hostID, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	deviceID, _ := identity.ParseDeviceID("device_0123456789abcdef0123456789abcdef")
	valid := model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: 1, ResolvedHash: "sha256:" + strings.Repeat("a", 64), HostID: hostID, DeviceIDs: []identity.DeviceID{deviceID}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	upper := valid
	upper.ResolvedHash = "sha256:" + strings.Repeat("A", 64)
	if err := upper.Validate(); err == nil {
		t.Fatal("uppercase non-canonical ResolvedHash accepted")
	}
	duplicate := valid
	duplicate.DeviceIDs = append(duplicate.DeviceIDs, deviceID)
	if err := duplicate.Validate(); err == nil {
		t.Fatal("duplicate DeviceID accepted")
	}
}

func TestDeviceConfigJSONPreservesOptionalValuesAndDeviceKeys(t *testing.T) {
	host, err := identity.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	device, err := identity.NewDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	zero := uint32(0)
	disabled := false
	original := model.DeviceConfig{
		HostID: host,
		CPU:    model.CPUConfig{Enabled: true, MiningThreads: &zero, HugePages: &disabled},
		GPUs:   map[identity.DeviceID]model.GPUConfig{device: {Enabled: true, PowerLimitW: &zero}},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded model.DeviceConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.HostID != host || !decoded.CPU.Enabled {
		t.Fatal("host or enable flag lost")
	}
	if decoded.CPU.MiningThreads == nil || *decoded.CPU.MiningThreads != 0 || decoded.CPU.HugePages == nil || *decoded.CPU.HugePages {
		t.Fatal("explicit zero/false lost")
	}
	if decoded.CPU.MaxThreads != nil || decoded.CPU.MSR != nil {
		t.Fatal("unspecified settings must stay nil")
	}
	gpu, ok := decoded.GPUs[device]
	if !ok || len(decoded.GPUs) != 1 || !gpu.Enabled || gpu.PowerLimitW == nil || *gpu.PowerLimitW != 0 || gpu.MaxPowerLimitW != nil {
		t.Fatal("GPU key or optional values lost")
	}
}

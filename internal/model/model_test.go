package model_test

import (
	"encoding/json"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

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

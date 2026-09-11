// Package processobserve performs conservative, observe-only Linux process discovery.
package processobserve

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

type Source struct {
	Root string
	Now  func() time.Time
}

func Local() Source { return Source{Root: "/", Now: time.Now} }

type ManagedInstance struct {
	PID             int
	ProcessInstance string
}

// Observe reads no process argv or environment. A process is reported only
// when its executable basename matches an adapter/provider signature.
func (source Source) Observe(inventory model.Inventory, signatures []model.ProcessSignature, managed []ManagedInstance) ([]model.UnmanagedProcessObservation, error) {
	if source.Root == "" {
		source.Root = "/"
	}
	if source.Now == nil {
		source.Now = time.Now
	}
	byExecutable := make(map[string][]model.ProcessSignature)
	for _, signature := range signatures {
		name := filepath.Base(strings.TrimSpace(signature.Executable))
		if name == "." || name == "" || signature.Provider == "" || (!signature.CPURelevant && !signature.GPURelevant) {
			continue
		}
		byExecutable[name] = append(byExecutable[name], signature)
	}
	entries, err := os.ReadDir(source.path("proc"))
	if err != nil {
		return nil, err
	}
	managedSet := make(map[string]struct{}, len(managed))
	for _, item := range managed {
		if item.PID > 0 && item.ProcessInstance != "" {
			managedSet[instanceKey(item.PID, item.ProcessInstance)] = struct{}{}
		}
	}
	observedAt := source.Now().UTC()
	var result []model.UnmanagedProcessObservation
	for _, entry := range entries {
		pid64, err := strconv.ParseInt(entry.Name(), 10, 32)
		if err != nil || pid64 < 1 || !entry.IsDir() {
			continue
		}
		pid := int(pid64)
		executablePath, err := os.Readlink(source.path("proc", entry.Name(), "exe"))
		if err != nil {
			continue
		}
		executable := filepath.Base(strings.TrimSuffix(executablePath, " (deleted)"))
		matches := byExecutable[executable]
		if len(matches) == 0 {
			continue
		}
		stat, err := os.ReadFile(source.path("proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		instance, err := parseProcessInstance(string(stat))
		if err != nil {
			continue
		}
		if _, owned := managedSet[instanceKey(pid, instance)]; owned {
			continue
		}
		item := model.UnmanagedProcessObservation{PID: pid, Executable: executable, ProcessInstance: instance, ObservedAt: observedAt}
		providers := make(map[string]struct{})
		for _, match := range matches {
			item.CPURelevant = item.CPURelevant || match.CPURelevant
			item.GPURelevant = item.GPURelevant || match.GPURelevant
			if _, exists := providers[match.Provider]; !exists {
				providers[match.Provider] = struct{}{}
				item.Evidence = append(item.Evidence, model.ProcessEvidence{Kind: "KNOWN_ADAPTER_EXECUTABLE", Provider: match.Provider, Detail: "executable basename matches a registered adapter"})
			}
		}
		item.DeviceIDs = source.gpuDevices(pid, inventory)
		switch {
		case item.CPURelevant && !item.GPURelevant:
			item.ResourceScope, item.CPU = model.ProcessResourcesExact, true
		case item.GPURelevant && !item.CPURelevant && len(item.DeviceIDs) > 0:
			item.ResourceScope = model.ProcessResourcesExact
		default:
			item.ResourceScope = model.ProcessResourcesUnknown
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].PID != result[j].PID {
			return result[i].PID < result[j].PID
		}
		return result[i].ProcessInstance < result[j].ProcessInstance
	})
	return result, nil
}

func (source Source) gpuDevices(pid int, inventory model.Inventory) []identity.DeviceID {
	entries, err := os.ReadDir(source.path("proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		return nil
	}
	byPCI := make(map[string]identity.DeviceID, len(inventory.GPUs))
	for _, gpu := range inventory.GPUs {
		byPCI[strings.ToLower(gpu.PCIBusID)] = gpu.DeviceID
	}
	seen := make(map[identity.DeviceID]struct{})
	for _, entry := range entries {
		target, err := os.Readlink(source.path("proc", strconv.Itoa(pid), "fd", entry.Name()))
		if err != nil || !strings.HasPrefix(target, "/dev/dri/") {
			continue
		}
		deviceName := filepath.Base(target)
		if !strings.HasPrefix(deviceName, "renderD") && !strings.HasPrefix(deviceName, "card") {
			continue
		}
		deviceTarget, err := os.Readlink(source.path("sys", "class", "drm", deviceName, "device"))
		if err != nil {
			continue
		}
		pci := strings.ToLower(filepath.Base(filepath.Clean(deviceTarget)))
		if id, ok := byPCI[pci]; ok && id.Validate() == nil {
			seen[id] = struct{}{}
		}
	}
	result := make([]identity.DeviceID, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	slices.SortFunc(result, func(a, b identity.DeviceID) int { return strings.Compare(a.String(), b.String()) })
	return result
}

func parseProcessInstance(stat string) (string, error) {
	close := strings.LastIndex(stat, ")")
	if close < 0 {
		return "", errors.New("process stat has no command terminator")
	}
	fields := strings.Fields(stat[close+1:])
	// fields starts at Linux proc stat field 3; starttime is field 22.
	if len(fields) <= 19 {
		return "", errors.New("process stat is incomplete")
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTicks == 0 {
		return "", fmt.Errorf("invalid process start ticks")
	}
	return "linux-proc-start-ticks:" + strconv.FormatUint(startTicks, 10), nil
}

func (source Source) path(parts ...string) string {
	all := append([]string{source.Root}, parts...)
	return filepath.Join(all...)
}

func instanceKey(pid int, instance string) string { return strconv.Itoa(pid) + "\x00" + instance }

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
	// readlink and readFile are narrow deterministic test seams for procfs
	// permission and process-exit races. Production uses the os functions.
	readlink func(string) (string, error)
	readFile func(string) ([]byte, error)
}

func Local() Source { return Source{Root: "/", Now: time.Now} }

type ManagedInstance struct {
	PID             int
	ProcessInstance string
}

// Observe reads no process argv or environment. A process is reported when
// its executable basename matches an adapter/provider signature, or when
// executable resolution is permission-denied and its comm matches exactly.
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
		stat, err := source.readFileAt(source.path("proc", entry.Name(), "stat"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read process %d stat: %w", pid, err)
		}
		instance, err := parseProcessInstance(string(stat))
		if err != nil {
			return nil, fmt.Errorf("parse process %d stat: %w", pid, err)
		}
		if _, owned := managedSet[instanceKey(pid, instance)]; owned {
			continue
		}

		executablePath, err := source.readLink(source.path("proc", entry.Name(), "exe"))
		executable := ""
		identityUnreadable := false
		switch {
		case err == nil:
			executable = filepath.Base(strings.TrimSuffix(executablePath, " (deleted)"))
		case errors.Is(err, os.ErrNotExist):
			// The process exited after procfs enumeration.
			continue
		case errors.Is(err, os.ErrPermission):
			// Default Ubuntu process protection can deny cross-UID exe
			// resolution while leaving the non-sensitive comm and stat facts
			// readable. An exact registered comm match is a conservative
			// candidate; unreadable identity never means a free resource.
			comm, commErr := source.readFileAt(source.path("proc", entry.Name(), "comm"))
			if errors.Is(commErr, os.ErrNotExist) {
				continue
			}
			if commErr != nil {
				return nil, fmt.Errorf("read process %d comm after executable access denial: %w", pid, commErr)
			}
			executable = strings.TrimSuffix(string(comm), "\n")
			if executable == "" || strings.ContainsAny(executable, "\r\n") {
				return nil, fmt.Errorf("process %d comm is invalid", pid)
			}
			identityUnreadable = true
		default:
			return nil, fmt.Errorf("resolve process %d executable: %w", pid, err)
		}
		matches := byExecutable[executable]
		if len(matches) == 0 {
			continue
		}
		item := model.UnmanagedProcessObservation{PID: pid, Executable: executable, ProcessInstance: instance, ObservedAt: observedAt}
		providers := make(map[string]struct{})
		for _, match := range matches {
			item.CPURelevant = item.CPURelevant || match.CPURelevant
			item.GPURelevant = item.GPURelevant || match.GPURelevant
			if _, exists := providers[match.Provider]; !exists {
				providers[match.Provider] = struct{}{}
				kind, detail := "KNOWN_ADAPTER_EXECUTABLE", "executable basename matches a registered adapter"
				if identityUnreadable {
					kind, detail = "REGISTERED_COMM_IDENTITY_UNREADABLE", "process comm exactly matches a registered adapter; executable identity is permission-denied"
				}
				item.Evidence = append(item.Evidence, model.ProcessEvidence{Kind: kind, Provider: match.Provider, Detail: detail})
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
		target, err := source.readLink(source.path("proc", strconv.Itoa(pid), "fd", entry.Name()))
		if err != nil || !strings.HasPrefix(target, "/dev/dri/") {
			continue
		}
		deviceName := filepath.Base(target)
		if !strings.HasPrefix(deviceName, "renderD") && !strings.HasPrefix(deviceName, "card") {
			continue
		}
		deviceTarget, err := source.readLink(source.path("sys", "class", "drm", deviceName, "device"))
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

func (source Source) readLink(path string) (string, error) {
	if source.readlink != nil {
		return source.readlink(path)
	}
	return os.Readlink(path)
}

func (source Source) readFileAt(path string) ([]byte, error) {
	if source.readFile != nil {
		return source.readFile(path)
	}
	return os.ReadFile(path)
}

func instanceKey(pid int, instance string) string { return strconv.Itoa(pid) + "\x00" + instance }

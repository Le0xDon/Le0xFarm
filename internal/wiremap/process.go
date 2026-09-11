package wiremap

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func UnmanagedProcesses(values []model.UnmanagedProcessObservation) *le0xv1.UnmanagedProcesses {
	out := &le0xv1.UnmanagedProcesses{Processes: make([]*le0xv1.UnmanagedProcess, 0, len(values))}
	for _, value := range values {
		item := &le0xv1.UnmanagedProcess{Pid: int64(value.PID), Executable: value.Executable, ProcessInstance: value.ProcessInstance, CpuRelevant: value.CPURelevant, GpuRelevant: value.GPURelevant, ResourceScope: string(value.ResourceScope), Cpu: value.CPU, ObservedAt: timestamppb.New(value.ObservedAt)}
		for _, id := range value.DeviceIDs {
			item.DeviceIds = append(item.DeviceIds, id.String())
		}
		for _, evidence := range value.Evidence {
			item.Evidence = append(item.Evidence, &le0xv1.ProcessEvidence{Kind: evidence.Kind, Provider: evidence.Provider, Detail: evidence.Detail})
		}
		out.Processes = append(out.Processes, item)
	}
	return out
}

func ParseUnmanagedProcesses(in *le0xv1.UnmanagedProcesses) ([]model.UnmanagedProcessObservation, error) {
	if in == nil || len(in.Processes) > 4096 {
		return nil, invalidProcess("unmanaged process observation is missing or too large")
	}
	result := make([]model.UnmanagedProcessObservation, 0, len(in.Processes))
	seen := make(map[string]struct{}, len(in.Processes))
	for _, wire := range in.Processes {
		if wire == nil || wire.Pid < 1 || wire.Pid > math.MaxInt || !safeToken(wire.Executable, 256) || filepath.Base(wire.Executable) != wire.Executable || !validProcessInstance(wire.ProcessInstance) || (!wire.CpuRelevant && !wire.GpuRelevant) || wire.ObservedAt == nil || !wire.ObservedAt.IsValid() || wire.ObservedAt.AsTime().IsZero() {
			return nil, invalidProcess("unmanaged process observation contains invalid process facts")
		}
		item := model.UnmanagedProcessObservation{PID: int(wire.Pid), Executable: wire.Executable, ProcessInstance: wire.ProcessInstance, CPURelevant: wire.CpuRelevant, GPURelevant: wire.GpuRelevant, ResourceScope: model.ProcessResourceAttribution(wire.ResourceScope), CPU: wire.Cpu, ObservedAt: wire.ObservedAt.AsTime().UTC()}
		if item.ResourceScope != model.ProcessResourcesExact && item.ResourceScope != model.ProcessResourcesUnknown {
			return nil, invalidProcess("unmanaged process resource attribution is invalid")
		}
		devices := make(map[identity.DeviceID]struct{}, len(wire.DeviceIds))
		for _, raw := range wire.DeviceIds {
			id, err := identity.ParseDeviceID(raw)
			if err != nil {
				return nil, invalidProcess("unmanaged process contains invalid DeviceID")
			}
			if _, duplicate := devices[id]; duplicate {
				return nil, invalidProcess("unmanaged process contains duplicate DeviceID")
			}
			devices[id] = struct{}{}
			item.DeviceIDs = append(item.DeviceIDs, id)
		}
		slices.SortFunc(item.DeviceIDs, func(a, b identity.DeviceID) int { return strings.Compare(a.String(), b.String()) })
		if item.ResourceScope == model.ProcessResourcesExact && !item.CPU && len(item.DeviceIDs) == 0 {
			return nil, invalidProcess("exact unmanaged process resource scope is empty")
		}
		if len(wire.Evidence) == 0 || len(wire.Evidence) > 16 {
			return nil, invalidProcess("unmanaged process evidence is missing or too large")
		}
		for _, evidence := range wire.Evidence {
			if evidence == nil || !safeToken(evidence.Kind, 64) || !safeToken(evidence.Provider, 128) || !safeText(evidence.Detail, 512) {
				return nil, invalidProcess("unmanaged process evidence is invalid")
			}
			item.Evidence = append(item.Evidence, model.ProcessEvidence{Kind: evidence.Kind, Provider: evidence.Provider, Detail: evidence.Detail})
		}
		key := fmt.Sprintf("%d\x00%s", item.PID, item.ProcessInstance)
		if _, duplicate := seen[key]; duplicate {
			return nil, invalidProcess("duplicate unmanaged process instance")
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	slices.SortFunc(result, func(a, b model.UnmanagedProcessObservation) int {
		if a.PID != b.PID {
			return a.PID - b.PID
		}
		return strings.Compare(a.ProcessInstance, b.ProcessInstance)
	})
	return result, nil
}

func validProcessInstance(value string) bool {
	const prefix = "linux-proc-start-ticks:"
	if !strings.HasPrefix(value, prefix) || len(value) > 64 {
		return false
	}
	ticks, err := strconv.ParseUint(strings.TrimPrefix(value, prefix), 10, 64)
	return err == nil && ticks > 0
}

func safeToken(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func safeText(value string, maximum int) bool {
	return len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func invalidProcess(message string) error {
	return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: message}
}

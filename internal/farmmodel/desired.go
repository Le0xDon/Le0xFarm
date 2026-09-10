package farmmodel

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

type DesiredRunState string

const (
	DesiredRunning DesiredRunState = "RUNNING"
	DesiredStopped DesiredRunState = "STOPPED"
)

type ResourceClaim struct {
	CPU       bool
	DeviceIDs []identity.DeviceID
}

type DesiredWorkloadContent struct {
	Name      string
	HostID    identity.HostID
	ProfileID identity.ProfileID
	RunState  DesiredRunState
	Worker    string
	Resources ResourceClaim
}

type DesiredWorkload struct {
	WorkloadID        identity.WorkloadID
	Meta              ObjectMeta
	DesiredGeneration uint64
	EffectiveHash     string
	DesiredWorkloadContent
}

// WorkloadRuntimeBinding contains only the durable safety latch needed to keep
// a terminally failed desired generation from being started in a loop.
type WorkloadRuntimeBinding struct {
	WorkloadID        identity.WorkloadID
	BlockedGeneration uint64
	ErrorCode         farmerr.Code
	HumanMessage      string
	BlockedAt         time.Time
}

// ResolvedRuntimeContent contains only runtime-effective desired intent. It
// deliberately excludes display names, source revisions and object IDs that do
// not affect execution. Source provenance is retained separately in snapshots.
type ResolvedRuntimeContent struct {
	RunState   DesiredRunState
	HostID     identity.HostID
	ProfileID  identity.ProfileID
	Resources  ResourceClaim
	AdapterID  string
	Package    PackageRef
	Mode       ProfileMode
	Coin       string
	Algorithm  string
	Endpoint   model.MiningEndpoint
	CPUThreads *uint32
	HugePages  *bool
	MSR        *bool
}

type ResolvedExecutionSnapshot struct {
	WorkloadID        identity.WorkloadID
	DesiredGeneration uint64
	ExecutionID       identity.ExecutionID
	HostID            identity.HostID
	ProfileID         identity.ProfileID
	ProfileRevision   uint64
	// HostProfileSettingsRevision is zero when no Host+Profile override existed.
	HostProfileSettingsRevision uint64
	PoolID                      identity.PoolID
	PoolRevision                uint64
	WalletID                    identity.WalletID
	WalletRevision              uint64
	Package                     PackageRef
	Resources                   ResourceClaim
	Plan                        model.ExecutionPlan
	ResolvedHash                string
	CreatedAt                   time.Time
}

func ValidateResourceClaim(claim ResourceClaim) error {
	if !claim.CPU && len(claim.DeviceIDs) == 0 {
		return invalid("ResourceClaim must include CPU or at least one DeviceID", nil)
	}
	seen := make(map[identity.DeviceID]struct{}, len(claim.DeviceIDs))
	for _, id := range claim.DeviceIDs {
		if err := id.Validate(); err != nil {
			return invalid("ResourceClaim contains an invalid DeviceID", err)
		}
		if _, exists := seen[id]; exists {
			return invalid("ResourceClaim contains a duplicate DeviceID", nil)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func NormalizeResourceClaim(claim ResourceClaim) ResourceClaim {
	normalized := ResourceClaim{CPU: claim.CPU, DeviceIDs: append([]identity.DeviceID(nil), claim.DeviceIDs...)}
	slices.SortFunc(normalized.DeviceIDs, func(a, b identity.DeviceID) int { return strings.Compare(a.String(), b.String()) })
	return normalized
}

func ValidateDesiredWorkload(content DesiredWorkloadContent) error {
	if err := validateName(content.Name); err != nil {
		return invalid("invalid DesiredWorkload name", err)
	}
	if err := content.HostID.Validate(); err != nil {
		return invalid("invalid DesiredWorkload HostID", err)
	}
	if err := content.ProfileID.Validate(); err != nil {
		return invalid("invalid DesiredWorkload ProfileID", err)
	}
	if content.RunState != DesiredRunning && content.RunState != DesiredStopped {
		return invalid("invalid desired run state", nil)
	}
	if len(content.Worker) > 256 || strings.ContainsAny(content.Worker, "\x00\r\n") || !utf8.ValidString(content.Worker) {
		return invalid("invalid workload worker", nil)
	}
	return ValidateResourceClaim(content.Resources)
}

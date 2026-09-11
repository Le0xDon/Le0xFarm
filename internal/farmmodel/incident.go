package farmmodel

import (
	"errors"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

type IncidentType string

const (
	IncidentAgentOffline               IncidentType = "AGENT_OFFLINE"
	IncidentMonitoringStale            IncidentType = "MONITORING_STALE"
	IncidentDesiredRunningNotExecuting IncidentType = "DESIRED_RUNNING_NOT_EXECUTING"
	IncidentRuntimeError               IncidentType = "RUNTIME_ERROR"
	IncidentUsefulWorkDegraded         IncidentType = "USEFUL_WORK_DEGRADED"
	IncidentTelemetryUnknown           IncidentType = "TELEMETRY_UNKNOWN"
	IncidentTelemetryUnavailable       IncidentType = "TELEMETRY_UNAVAILABLE"
	IncidentIncompatibleHardware       IncidentType = "INCOMPATIBLE_HARDWARE"
	IncidentUnmanagedProcessConflict   IncidentType = "UNMANAGED_PROCESS_CONFLICT"
	IncidentResourceConflict           IncidentType = "RESOURCE_CONFLICT"
	IncidentMaintenanceHold            IncidentType = "MAINTENANCE_HOLD"
	IncidentConfigurationBlocked       IncidentType = "CONFIGURATION_BLOCKED"
)

type IncidentSeverity string

const (
	IncidentSeverityInfo     IncidentSeverity = "INFO"
	IncidentSeverityWarning  IncidentSeverity = "WARNING"
	IncidentSeverityError    IncidentSeverity = "ERROR"
	IncidentSeverityCritical IncidentSeverity = "CRITICAL"
)

type IncidentState string

const (
	IncidentActive   IncidentState = "ACTIVE"
	IncidentResolved IncidentState = "RESOLVED"
)

type IncidentSource string

const IncidentSourceController IncidentSource = "CONTROLLER"

// IncidentCondition is a normalized, non-secret fact produced by Controller
// evaluation. Key and IncidentID are deterministic from typed scope; they do
// not contain timestamps, PIDs, or human messages.
type IncidentCondition struct {
	IncidentID  identity.IncidentID
	Key         string
	Type        IncidentType
	Severity    IncidentSeverity
	HostID      identity.HostID
	WorkloadID  *identity.WorkloadID
	ExecutionID *identity.ExecutionID
	DeviceID    *identity.DeviceID
	ReasonCode  farmerr.Code
	Source      IncidentSource
}

type Incident struct {
	IncidentCondition
	State           IncidentState
	FirstObservedAt time.Time
	LastObservedAt  time.Time
	ResolvedAt      *time.Time
	OccurrenceCount uint64
}

type IncidentQuery struct {
	HostID *identity.HostID
	State  IncidentState
	Limit  int
}

func ValidIncidentType(value IncidentType) bool {
	switch value {
	case IncidentAgentOffline, IncidentMonitoringStale, IncidentDesiredRunningNotExecuting,
		IncidentRuntimeError, IncidentUsefulWorkDegraded, IncidentTelemetryUnknown, IncidentTelemetryUnavailable,
		IncidentIncompatibleHardware, IncidentUnmanagedProcessConflict, IncidentResourceConflict,
		IncidentMaintenanceHold, IncidentConfigurationBlocked:
		return true
	default:
		return false
	}
}

func ValidIncidentSeverity(value IncidentSeverity) bool {
	switch value {
	case IncidentSeverityInfo, IncidentSeverityWarning, IncidentSeverityError, IncidentSeverityCritical:
		return true
	default:
		return false
	}
}

func ValidateIncidentCondition(value IncidentCondition) error {
	if err := value.IncidentID.Validate(); err != nil || len(value.Key) == 0 || len(value.Key) > 512 {
		return errors.New("invalid incident identity")
	}
	if !ValidIncidentType(value.Type) || !ValidIncidentSeverity(value.Severity) || value.Source != IncidentSourceController {
		return errors.New("invalid incident classification")
	}
	if !farmerr.ValidCode(value.ReasonCode) {
		return errors.New("invalid incident reason code")
	}
	if err := value.HostID.Validate(); err != nil {
		return errors.New("invalid incident HostID")
	}
	if value.WorkloadID != nil {
		if err := value.WorkloadID.Validate(); err != nil {
			return errors.New("invalid incident WorkloadID")
		}
	}
	if value.ExecutionID != nil {
		if err := value.ExecutionID.Validate(); err != nil {
			return errors.New("invalid incident ExecutionID")
		}
	}
	if value.DeviceID != nil {
		if err := value.DeviceID.Validate(); err != nil {
			return errors.New("invalid incident DeviceID")
		}
	}
	return nil
}

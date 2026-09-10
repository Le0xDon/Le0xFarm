// Package model contains foundation domain data, without runtime behavior.
package model

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"

	farmerr "github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/protocol"
)

// Inventory describes physically installed hardware, not usage permissions.
type Inventory struct {
	SchemaVersion protocol.SchemaVersion
	Host          Host
	CPU           CPU
	GPUs          []GPU
	Memory        Memory
}

type Host struct {
	HostID       identity.HostID
	Hostname     string
	DisplayName  string
	OS           OSInfo
	Architecture string
}

type OSInfo struct {
	ID         string
	VersionID  string
	PrettyName string
	Kernel     string
}

type CPU struct {
	Vendor  string
	Model   string
	Sockets uint32
	Cores   uint32
	Threads uint32
}

type GPU struct {
	// DeviceID must be persisted by a future inventory implementation across scans.
	DeviceID identity.DeviceID
	Vendor   string
	Model    string
	PCIBusID string
	UUID     string // Optional vendor-provided GPU UUID.
}

type Memory struct{ TotalBytes uint64 }

// DeviceConfig defines usage permissions separately from physical inventory.
// A GPU absent from GPUs is not enabled for use.
type DeviceConfig struct {
	HostID identity.HostID
	CPU    CPUConfig
	GPUs   map[identity.DeviceID]GPUConfig
}

// Nil optional settings mean unspecified / auto / inherit.
type CPUConfig struct {
	Enabled       bool
	MiningThreads *uint32
	MaxThreads    *uint32
	HugePages     *bool
	MSR           *bool
}

// Nil optional settings mean unspecified / auto / inherit.
type GPUConfig struct {
	Enabled        bool
	PowerLimitW    *uint32
	MaxPowerLimitW *uint32
}

// WalletRef contains only a public payout address, never a seed, private key or mnemonic.
type WalletRef struct {
	WalletID identity.WalletID
	Name     string
	Coin     string
	Address  string
}

type Pool struct {
	PoolID identity.PoolID
	Name   string
	Coin   string
	URL    string
}

// Package describes an artifact, without installation or execution behavior.
type Package struct {
	PackageID identity.PackageID
	Name      string
	Version   string
	SHA256    string
}

type MiningProfile struct {
	ProfileID identity.ProfileID
	Name      string
	Coin      string
	PackageID identity.PackageID
	WalletID  identity.WalletID
	PoolID    identity.PoolID
}

type ServiceProfile struct {
	ServiceID identity.ServiceID
	Name      string
	Kind      string
	Coin      string
	PackageID identity.PackageID
}

type DesiredState struct {
	SchemaVersion protocol.SchemaVersion
	HostID        identity.HostID
	Revision      uint64
	Mining        []DesiredMining
	Services      []identity.ServiceID
}

type DesiredMining struct {
	ProfileID identity.ProfileID
	DeviceIDs []identity.DeviceID
}

type AgentState string

const (
	AgentStateIdle     AgentState = "IDLE"
	AgentStateStarting AgentState = "STARTING"
	AgentStateMining   AgentState = "MINING"
	AgentStateDegraded AgentState = "DEGRADED"
	AgentStateError    AgentState = "ERROR"
)

type ExecutionStatus string

const (
	ExecutionStarting ExecutionStatus = "STARTING"
	ExecutionRunning  ExecutionStatus = "RUNNING"
	ExecutionStopping ExecutionStatus = "STOPPING"
	ExecutionStopped  ExecutionStatus = "STOPPED"
	ExecutionCrashed  ExecutionStatus = "CRASHED"
	ExecutionBackoff  ExecutionStatus = "BACKOFF"
	ExecutionFailed   ExecutionStatus = "FAILED"
)

type ObservedState struct {
	SchemaVersion   protocol.SchemaVersion
	HostID          identity.HostID
	ObservedAt      time.Time
	AppliedRevision uint64
	Executions      []ExecutionObservation
}

type ExecutionObservation struct {
	ExecutionID    identity.ExecutionID
	Ownership      *WorkloadOwnership
	Status         ExecutionStatus
	Error          *farmerr.Error
	PID            int
	StartedAt      time.Time
	ExitCode       *int
	RestartCount   uint32
	LastError      string
	MinerTelemetry *MinerTelemetry
}

// WorkloadOwnership is the adapter-neutral Controller snapshot identity that
// an Agent retains verbatim for the lifetime of an execution. A nil ownership
// on an observation means that the execution is unmanaged by this Controller.
type WorkloadOwnership struct {
	WorkloadID        identity.WorkloadID
	DesiredGeneration uint64
	ResolvedHash      string
	HostID            identity.HostID
	CPU               bool
	DeviceIDs         []identity.DeviceID
}

func (ownership WorkloadOwnership) Validate() error {
	if err := ownership.WorkloadID.Validate(); err != nil {
		return errors.New("invalid WorkloadID")
	}
	if ownership.DesiredGeneration < 1 {
		return errors.New("DesiredGeneration must be at least 1")
	}
	if err := ownership.HostID.Validate(); err != nil {
		return errors.New("invalid HostID")
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(ownership.ResolvedHash, prefix) || len(ownership.ResolvedHash) != len(prefix)+64 {
		return errors.New("invalid ResolvedHash")
	}
	rawHash := strings.TrimPrefix(ownership.ResolvedHash, prefix)
	if rawHash != strings.ToLower(rawHash) {
		return errors.New("invalid ResolvedHash")
	}
	if _, err := hex.DecodeString(rawHash); err != nil {
		return errors.New("invalid ResolvedHash")
	}
	if !ownership.CPU && len(ownership.DeviceIDs) == 0 {
		return errors.New("ResourceClaim must include CPU or at least one DeviceID")
	}
	seen := make(map[identity.DeviceID]struct{}, len(ownership.DeviceIDs))
	for _, deviceID := range ownership.DeviceIDs {
		if err := deviceID.Validate(); err != nil {
			return errors.New("invalid ResourceClaim DeviceID")
		}
		if _, exists := seen[deviceID]; exists {
			return errors.New("duplicate ResourceClaim DeviceID")
		}
		seen[deviceID] = struct{}{}
	}
	return nil
}

type ExecutionPlan struct {
	SchemaVersion    protocol.SchemaVersion
	ExecutionID      identity.ExecutionID
	Ownership        WorkloadOwnership
	HostID           identity.HostID
	ProfileID        identity.ProfileID
	DeviceIDs        []identity.DeviceID
	CPUThreads       *uint32 // Nil means unspecified / auto / inherit.
	Executable       string
	Args             []string
	Environment      map[string]string
	WorkingDirectory string
	RestartPolicy    RestartPolicy
	Miner            *MinerSpec
}

type MinerMode string

const (
	MinerModeMining    MinerMode = "MINING"
	MinerModeStress    MinerMode = "STRESS"
	MinerModeBenchmark MinerMode = "BENCHMARK"
)

// MinerSpec is adapter-neutral. Options is a versioned extension namespace;
// adapters validate every key they consume.
type MinerSpec struct {
	AdapterID      string
	SpecVersion    uint32
	PackageID      identity.PackageID
	PackageVersion string
	WalletID       *identity.WalletID
	PoolID         *identity.PoolID
	Mode           MinerMode
	Coin           string
	Algorithm      string
	Endpoint       *MiningEndpoint
	CPUThreads     *uint32
	GPUDeviceIDs   []identity.DeviceID
	HugePages      *bool
	MSR            *bool
	Options        map[string]string
}

// MiningEndpoint is the resolved connection snapshot sent to an Agent. User is
// the exact miner-facing pool login; it may be a public payout address or a
// pool account identity. Worker is separate and is never implicitly appended
// to User. Password is potentially sensitive and must not be logged.
type MiningEndpoint struct {
	Address  string
	TLS      bool
	User     string
	Password string
	Worker   string
}

type MinerHealth string

const (
	MinerHealthStarting MinerHealth = "STARTING"
	MinerHealthHealthy  MinerHealth = "HEALTHY"
	MinerHealthMining   MinerHealth = "MINING"
	MinerHealthDegraded MinerHealth = "DEGRADED"
	MinerHealthError    MinerHealth = "ERROR"
)

type DeviceHashrate struct {
	DeviceID    identity.DeviceID
	HashrateHPS float64
}

type MinerTelemetry struct {
	AdapterID          string
	MinerVersion       string
	Algorithm          string
	HashrateShortHPS   *float64
	HashrateMediumHPS  *float64
	HashrateLongHPS    *float64
	HighestHashrateHPS *float64
	PerDevice          []DeviceHashrate
	AcceptedShares     *uint64
	RejectedShares     *uint64
	StaleShares        *uint64
	TotalResults       *uint64
	PoolConnected      *bool
	PoolLatencyMS      *uint32
	UptimeSeconds      uint64
	HugePagesAvailable *bool
	HugePagesPercent   *float64
	MSRAvailable       *bool
	CollectedAt        time.Time
	Age                time.Duration
	Health             MinerHealth
	ErrorCode          farmerr.Code
	Message            string
}

type RestartPolicy string

const (
	RestartNever     RestartPolicy = "NEVER"
	RestartOnFailure RestartPolicy = "ON_FAILURE"
)

type Event struct {
	SchemaVersion protocol.SchemaVersion
	HostID        identity.HostID
	OccurredAt    time.Time
	Kind          string
	Message       string
	Error         *farmerr.Error
}

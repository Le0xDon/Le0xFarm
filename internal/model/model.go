// Package model contains foundation domain data, without runtime behavior.
package model

import (
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

type ExecutionStatus string

const (
	ExecutionPending ExecutionStatus = "pending"
	ExecutionRunning ExecutionStatus = "running"
	ExecutionStopped ExecutionStatus = "stopped"
	ExecutionFailed  ExecutionStatus = "failed"
)

type ObservedState struct {
	SchemaVersion   protocol.SchemaVersion
	HostID          identity.HostID
	ObservedAt      time.Time
	AppliedRevision uint64
	Executions      []ExecutionObservation
}

type ExecutionObservation struct {
	ExecutionID identity.ExecutionID
	Status      ExecutionStatus
	Error       *farmerr.Error
}

// ExecutionPlan links an intended execution to its profile and allowed devices.
// It does not yet contain process commands or orchestration logic.
type ExecutionPlan struct {
	SchemaVersion protocol.SchemaVersion
	ExecutionID   identity.ExecutionID
	HostID        identity.HostID
	ProfileID     identity.ProfileID
	DeviceIDs     []identity.DeviceID
	CPUThreads    *uint32 // Nil means unspecified / auto / inherit.
}

type Event struct {
	SchemaVersion protocol.SchemaVersion
	HostID        identity.HostID
	OccurredAt    time.Time
	Kind          string
	Message       string
	Error         *farmerr.Error
}

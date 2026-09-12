package farmmodel

import (
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

type RestoreBarrierStatus string

const (
	RestoreBarrierPending  RestoreBarrierStatus = "PENDING"
	RestoreBarrierConflict RestoreBarrierStatus = "CONFLICT"
)

type ControllerRestoreState struct {
	Required               bool
	BackupID               string
	SourceControllerID     identity.ControllerID
	SourceFarmID           identity.FarmID
	SourceTrustFingerprint string
	RestoredAt             time.Time
}

type RestoreHostBarrier struct {
	HostID     identity.HostID
	BackupID   string
	Revision   uint64
	Status     RestoreBarrierStatus
	ReasonCode farmerr.Code
	UpdatedAt  time.Time
}

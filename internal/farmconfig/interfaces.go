package farmconfig

import (
	"context"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

// Pools, WalletRefs and MiningProfiles are transport-independent application
// boundaries. A future CLI, Web UI or Brain integration can call these without
// owning SQLite or bypassing validation and optimistic concurrency.
type Pools interface {
	CreatePool(context.Context, farmmodel.PoolContent) (farmmodel.Pool, error)
	GetPool(context.Context, identity.PoolID) (farmmodel.Pool, error)
	ListPools(context.Context) ([]farmmodel.Pool, error)
	UpdatePool(context.Context, identity.PoolID, uint64, farmmodel.PoolContent) (farmmodel.Pool, error)
	DeletePool(context.Context, identity.PoolID, uint64) error
}

type WalletRefs interface {
	CreateWalletRef(context.Context, farmmodel.WalletRefContent) (farmmodel.WalletRef, error)
	GetWalletRef(context.Context, identity.WalletID) (farmmodel.WalletRef, error)
	ListWalletRefs(context.Context) ([]farmmodel.WalletRef, error)
	UpdateWalletRef(context.Context, identity.WalletID, uint64, farmmodel.WalletRefContent) (farmmodel.WalletRef, error)
	DeleteWalletRef(context.Context, identity.WalletID, uint64) error
}

type MiningProfiles interface {
	CreateMiningProfile(context.Context, farmmodel.MiningProfileContent) (farmmodel.MiningProfile, error)
	GetMiningProfile(context.Context, identity.ProfileID) (farmmodel.MiningProfile, error)
	ListMiningProfiles(context.Context) ([]farmmodel.MiningProfile, error)
	UpdateMiningProfile(context.Context, identity.ProfileID, uint64, farmmodel.MiningProfileContent) (farmmodel.MiningProfile, error)
	DeleteMiningProfile(context.Context, identity.ProfileID, uint64) error
}

type HostProfileSettings interface {
	CreateHostProfileSettings(context.Context, identity.HostID, identity.ProfileID, farmmodel.HostProfileSettingsContent) (farmmodel.HostProfileSettings, error)
	GetHostProfileSettings(context.Context, identity.HostID, identity.ProfileID) (farmmodel.HostProfileSettings, error)
	ListHostProfileSettings(context.Context) ([]farmmodel.HostProfileSettings, error)
	UpdateHostProfileSettings(context.Context, identity.HostID, identity.ProfileID, uint64, farmmodel.HostProfileSettingsContent) (farmmodel.HostProfileSettings, error)
	DeleteHostProfileSettings(context.Context, identity.HostID, identity.ProfileID, uint64) error
}

type DesiredWorkloads interface {
	CreateDesiredWorkload(context.Context, farmmodel.DesiredWorkloadContent) (farmmodel.DesiredWorkload, error)
	GetDesiredWorkload(context.Context, identity.WorkloadID) (farmmodel.DesiredWorkload, error)
	ListDesiredWorkloads(context.Context) ([]farmmodel.DesiredWorkload, error)
	UpdateDesiredWorkload(context.Context, identity.WorkloadID, uint64, farmmodel.DesiredWorkloadContent) (farmmodel.DesiredWorkload, error)
	DeleteDesiredWorkload(context.Context, identity.WorkloadID, uint64) error
	GetCurrentResolvedSnapshot(context.Context, identity.WorkloadID) (farmmodel.ResolvedExecutionSnapshot, error)
	ListResolvedSnapshots(context.Context, identity.WorkloadID) ([]farmmodel.ResolvedExecutionSnapshot, error)
	ListAllResolvedSnapshots(context.Context) ([]farmmodel.ResolvedExecutionSnapshot, error)
	BlockDesiredGeneration(context.Context, identity.WorkloadID, uint64, farmerr.Code, string) error
	GetWorkloadRuntimeBinding(context.Context, identity.WorkloadID) (farmmodel.WorkloadRuntimeBinding, bool, error)
	RetryWorkload(context.Context, identity.WorkloadID, uint64) (farmmodel.DesiredWorkload, error)
	RefreshResolvedSnapshotForInventory(context.Context, identity.WorkloadID, model.Inventory) error
}

var _ interface {
	Pools
	WalletRefs
	MiningProfiles
	HostProfileSettings
	DesiredWorkloads
} = (*Service)(nil)

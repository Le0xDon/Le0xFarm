// Package farmresolve deterministically resolves reusable farm objects into
// runtime intent without sending commands or consulting Agent state.
package farmresolve

import (
	"context"
	"strings"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/protocol"
)

type Resolver struct{ Catalog farmmodel.PackageCatalog }

type Inputs struct {
	Workload farmmodel.DesiredWorkload
	Profile  farmmodel.MiningProfile
	Pool     farmmodel.Pool
	Wallet   farmmodel.WalletRef
}

type Result struct {
	Content farmmodel.ResolvedRuntimeContent
	Hash    string
}

func (resolver Resolver) Resolve(ctx context.Context, inputs Inputs) (Result, error) {
	if resolver.Catalog == nil {
		return Result{}, typed(farmerr.CONFIG_CONFLICT, "package catalog is required")
	}
	if err := farmmodel.ValidateDesiredWorkload(inputs.Workload.DesiredWorkloadContent); err != nil {
		return Result{}, err
	}
	if err := farmmodel.ValidateMiningProfile(inputs.Profile.MiningProfileContent); err != nil {
		return Result{}, err
	}
	if err := farmmodel.ValidatePool(inputs.Pool.PoolContent); err != nil {
		return Result{}, err
	}
	if err := farmmodel.ValidateWalletRef(inputs.Wallet.WalletRefContent); err != nil {
		return Result{}, err
	}
	if inputs.Workload.ProfileID != inputs.Profile.ProfileID || inputs.Profile.PoolID != inputs.Pool.PoolID || inputs.Profile.WalletID != inputs.Wallet.WalletID {
		return Result{}, typed(farmerr.INVALID_REFERENCE, "resolver object references do not agree")
	}
	release, err := resolver.Catalog.Lookup(ctx, inputs.Profile.Package)
	if err != nil {
		return Result{}, typed(farmerr.INVALID_REFERENCE, "package release is unavailable")
	}
	if release.Ref != inputs.Profile.Package {
		return Result{}, typed(farmerr.INVALID_REFERENCE, "package catalog returned a different release")
	}
	if inputs.Wallet.Coin != inputs.Profile.Coin {
		return Result{}, typed(farmerr.INVALID_REFERENCE, "WalletRef coin does not match MiningProfile coin")
	}
	found := false
	for _, adapter := range release.AdapterIDs {
		if adapter == inputs.Profile.AdapterID {
			found = true
			break
		}
	}
	if !found {
		return Result{}, typed(farmerr.INVALID_REFERENCE, "package release does not support profile adapter")
	}
	endpoint, err := resolveEndpoint(inputs.Pool, inputs.Wallet, inputs.Profile.LoginPolicy, inputs.Workload.Worker)
	if err != nil {
		return Result{}, err
	}
	content := farmmodel.ResolvedRuntimeContent{
		RunState: inputs.Workload.RunState, HostID: inputs.Workload.HostID, ProfileID: inputs.Profile.ProfileID,
		Resources: farmmodel.NormalizeResourceClaim(inputs.Workload.Resources), AdapterID: inputs.Profile.AdapterID,
		Package: inputs.Profile.Package, Mode: inputs.Profile.Mode, Coin: inputs.Profile.Coin,
		Algorithm: inputs.Profile.Algorithm, Endpoint: endpoint, CPUThreads: cloneUint32(inputs.Profile.CPUThreads),
		HugePages: cloneBool(inputs.Profile.HugePages), MSR: cloneBool(inputs.Profile.MSR),
	}
	hash, err := farmmodel.ResolvedRuntimeHash(content)
	if err != nil {
		return Result{}, typed(farmerr.INTERNAL_ERROR, "cannot hash resolved runtime intent")
	}
	return Result{Content: content, Hash: hash}, nil
}

func (resolver Resolver) Snapshot(result Result, inputs Inputs, executionID identity.ExecutionID, generation uint64, createdAt time.Time) (farmmodel.ResolvedExecutionSnapshot, error) {
	if result.Content.RunState != farmmodel.DesiredRunning || executionID.Validate() != nil || generation < 1 {
		return farmmodel.ResolvedExecutionSnapshot{}, typed(farmerr.CONFIG_CONFLICT, "invalid running snapshot identity")
	}
	poolID, walletID := inputs.Profile.PoolID, inputs.Profile.WalletID
	plan := model.ExecutionPlan{SchemaVersion: protocol.CurrentSchemaVersion, ExecutionID: executionID,
		Ownership: model.WorkloadOwnership{WorkloadID: inputs.Workload.WorkloadID, DesiredGeneration: generation, ResolvedHash: result.Hash, HostID: inputs.Workload.HostID,
			CPU: result.Content.Resources.CPU, DeviceIDs: append([]identity.DeviceID(nil), result.Content.Resources.DeviceIDs...)},
		HostID: inputs.Workload.HostID, ProfileID: inputs.Profile.ProfileID,
		DeviceIDs: append([]identity.DeviceID(nil), result.Content.Resources.DeviceIDs...), CPUThreads: cloneUint32(result.Content.CPUThreads), RestartPolicy: model.RestartOnFailure,
		Miner: &model.MinerSpec{AdapterID: result.Content.AdapterID, SpecVersion: 1, PackageID: result.Content.Package.PackageID, PackageVersion: result.Content.Package.Version,
			WalletID: &walletID, PoolID: &poolID, Mode: model.MinerModeMining, Coin: result.Content.Coin, Algorithm: result.Content.Algorithm,
			Endpoint:   &model.MiningEndpoint{Address: result.Content.Endpoint.Address, TLS: result.Content.Endpoint.TLS, User: result.Content.Endpoint.User, Password: result.Content.Endpoint.Password, Worker: result.Content.Endpoint.Worker},
			CPUThreads: cloneUint32(result.Content.CPUThreads), GPUDeviceIDs: append([]identity.DeviceID(nil), result.Content.Resources.DeviceIDs...), HugePages: cloneBool(result.Content.HugePages), MSR: cloneBool(result.Content.MSR)}}
	return farmmodel.ResolvedExecutionSnapshot{WorkloadID: inputs.Workload.WorkloadID, DesiredGeneration: generation, ExecutionID: executionID, HostID: inputs.Workload.HostID,
		ProfileID: inputs.Profile.ProfileID, ProfileRevision: inputs.Profile.Meta.Revision, PoolID: inputs.Pool.PoolID, PoolRevision: inputs.Pool.Meta.Revision,
		WalletID: inputs.Wallet.WalletID, WalletRevision: inputs.Wallet.Meta.Revision, Package: inputs.Profile.Package,
		Resources: farmmodel.NormalizeResourceClaim(inputs.Workload.Resources), Plan: plan, ResolvedHash: result.Hash, CreatedAt: createdAt.UTC()}, nil
}

func resolveEndpoint(pool farmmodel.Pool, wallet farmmodel.WalletRef, policy farmmodel.LoginPolicy, worker string) (model.MiningEndpoint, error) {
	if err := farmmodel.ValidateLoginPolicy(policy); err != nil {
		return model.MiningEndpoint{}, err
	}
	user := strings.Replace(policy.UserTemplate, "${wallet}", wallet.Address, 1)
	resolvedWorker := ""
	switch policy.WorkerPlacement {
	case farmmodel.WorkerNone:
		if worker != "" {
			return model.MiningEndpoint{}, typed(farmerr.CONFIG_CONFLICT, "worker must be empty for NONE placement")
		}
	case farmmodel.WorkerInUser:
		if worker == "" {
			return model.MiningEndpoint{}, typed(farmerr.CONFIG_CONFLICT, "worker is required for IN_USER placement")
		}
		user = strings.Replace(user, "${worker}", worker, 1)
	case farmmodel.WorkerSeparate:
		if worker == "" {
			return model.MiningEndpoint{}, typed(farmerr.CONFIG_CONFLICT, "worker is required for SEPARATE placement")
		}
		resolvedWorker = worker
	}
	password := ""
	if pool.Auth.Kind == farmmodel.PoolAuthPublicLiteral {
		password = pool.Auth.PublicLiteral
	}
	return model.MiningEndpoint{Address: pool.Address, TLS: pool.TLS, User: user, Password: password, Worker: resolvedWorker}, nil
}

func cloneUint32(value *uint32) *uint32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func typed(code farmerr.Code, message string) error {
	return farmerr.Error{Code: code, HumanMessage: message}
}

// Package farmresolve deterministically resolves reusable farm objects into
// runtime intent without sending commands or consulting Agent state.
package farmresolve

import (
	"context"
	"fmt"
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
	Settings *farmmodel.HostProfileSettings
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
	if inputs.Settings != nil {
		if inputs.Settings.HostID != inputs.Workload.HostID || inputs.Settings.ProfileID != inputs.Profile.ProfileID {
			return Result{}, typed(farmerr.INVALID_REFERENCE, "HostProfileSettings does not match workload HostID and ProfileID")
		}
		if err := farmmodel.ValidateHostProfileSettings(inputs.Settings.HostID, inputs.Settings.ProfileID, inputs.Settings.HostProfileSettingsContent); err != nil {
			return Result{}, err
		}
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
	cpuThreads, hugePages, msr := effectiveTuning(inputs.Profile, inputs.Settings)
	if err := farmmodel.ValidateTuningCapabilities(cpuThreads, hugePages, msr, release.Tuning); err != nil {
		return Result{}, err
	}
	endpoint, err := resolveEndpoint(inputs.Pool, inputs.Wallet, inputs.Profile.LoginPolicy, inputs.Workload.Worker)
	if err != nil {
		return Result{}, err
	}
	content := farmmodel.ResolvedRuntimeContent{
		RunState: inputs.Workload.RunState, HostID: inputs.Workload.HostID, ProfileID: inputs.Profile.ProfileID,
		Resources: farmmodel.NormalizeResourceClaim(inputs.Workload.Resources), AdapterID: inputs.Profile.AdapterID,
		Package: inputs.Profile.Package, Mode: inputs.Profile.Mode, Coin: inputs.Profile.Coin,
		Algorithm: inputs.Profile.Algorithm, Endpoint: endpoint, CPUThreads: cpuThreads,
		HugePages: hugePages, MSR: msr,
	}
	hash, err := farmmodel.ResolvedRuntimeHash(content)
	if err != nil {
		return Result{}, typed(farmerr.INTERNAL_ERROR, "cannot hash resolved runtime intent")
	}
	return Result{Content: content, Hash: hash}, nil
}

// ResolveAndValidate applies the same deterministic precedence as Resolve and
// then fails closed against current Host hardware. Freshness is established by
// the Controller observation layer before this method is called.
func (resolver Resolver) ResolveAndValidate(ctx context.Context, inputs Inputs, inventory model.Inventory) (Result, error) {
	result, err := resolver.Resolve(ctx, inputs)
	if err != nil {
		return Result{}, err
	}
	if inventory.Host.HostID != inputs.Workload.HostID {
		return Result{}, hardwareError(inputs, "fresh inventory belongs to a different Host", nil)
	}
	if result.Content.Resources.CPU {
		if inventory.CPU.Threads == 0 {
			return Result{}, hardwareError(inputs, "fresh inventory does not report logical CPU capacity", nil)
		}
		if result.Content.CPUThreads != nil && *result.Content.CPUThreads > inventory.CPU.Threads {
			return Result{}, hardwareError(inputs, fmt.Sprintf("requests CPUThreads=%d but fresh inventory supports %d", *result.Content.CPUThreads, inventory.CPU.Threads), map[string]string{
				"requested": fmt.Sprint(*result.Content.CPUThreads), "supported": fmt.Sprint(inventory.CPU.Threads),
			})
		}
	}
	available := make(map[identity.DeviceID]struct{}, len(inventory.GPUs))
	for _, gpu := range inventory.GPUs {
		available[gpu.DeviceID] = struct{}{}
	}
	for _, deviceID := range result.Content.Resources.DeviceIDs {
		if _, ok := available[deviceID]; !ok {
			return Result{}, hardwareError(inputs, "requested DeviceID is absent from fresh inventory", map[string]string{"device_id": deviceID.String()})
		}
	}
	return result, nil
}

func effectiveTuning(profile farmmodel.MiningProfile, settings *farmmodel.HostProfileSettings) (*uint32, *bool, *bool) {
	cpuThreads := cloneUint32(profile.CPUThreads)
	hugePages := cloneBool(profile.HugePages)
	msr := cloneBool(profile.MSR)
	if settings == nil {
		return cpuThreads, hugePages, msr
	}
	if settings.CPUThreads != nil {
		cpuThreads = cloneUint32(settings.CPUThreads)
	}
	if settings.HugePages != nil {
		hugePages = cloneBool(settings.HugePages)
	}
	if settings.MSR != nil {
		msr = cloneBool(settings.MSR)
	}
	return cpuThreads, hugePages, msr
}

func hardwareError(inputs Inputs, message string, extra map[string]string) error {
	details := map[string]string{"host_id": inputs.Workload.HostID.String(), "profile_id": inputs.Profile.ProfileID.String()}
	for key, value := range extra {
		details[key] = value
	}
	return farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: fmt.Sprintf("Host %s Profile %s: %s", inputs.Workload.HostID, inputs.Profile.ProfileID, message), Details: details}
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
		ProfileID: inputs.Profile.ProfileID, ProfileRevision: inputs.Profile.Meta.Revision, HostProfileSettingsRevision: settingsRevision(inputs.Settings), PoolID: inputs.Pool.PoolID, PoolRevision: inputs.Pool.Meta.Revision,
		WalletID: inputs.Wallet.WalletID, WalletRevision: inputs.Wallet.Meta.Revision, Package: inputs.Profile.Package,
		Resources: farmmodel.NormalizeResourceClaim(inputs.Workload.Resources), Plan: plan, ResolvedHash: result.Hash, CreatedAt: createdAt.UTC()}, nil
}

func settingsRevision(settings *farmmodel.HostProfileSettings) uint64 {
	if settings == nil {
		return 0
	}
	return settings.Meta.Revision
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

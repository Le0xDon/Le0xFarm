// Package minerruntime connects miner adapters to the generic process supervisor.
package minerruntime

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/gpuresource"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/packages"
	"github.com/le0xdon/le0xfarm/internal/processobserve"
	"github.com/le0xdon/le0xfarm/internal/runtime/supervisor"
)

type Capabilities struct {
	CPU                   bool
	GPU                   bool
	MultiGPUSingleProcess bool
	PerGPUProcess         bool
	HTTPAPI               bool
	StdoutTelemetry       bool
	Benchmark             bool
	Stress                bool
	Algorithms            []string
	GPUVendors            []string
}

type PrepareRequest struct {
	Plan         model.ExecutionPlan
	AgentDataDir string
	Packages     PackageResolver
	Inventory    model.Inventory
}

// PackageResolver exposes only verified installed artifacts to adapters.
// packages.Store implements this contract; adapters never install or download.
type PackageResolver interface {
	Lookup(identity.PackageID, string) (packages.Installed, error)
}

type Prepared struct {
	Plan      model.ExecutionPlan
	Telemetry TelemetrySource
	Cleanup   func() error
}

type TelemetrySource interface {
	Poll(context.Context) (*model.MinerTelemetry, error)
}

type Adapter interface {
	ID() string
	Capabilities() Capabilities
	Validate(model.MinerSpec, model.Inventory) ([]string, error)
	Prepare(context.Context, PrepareRequest) (Prepared, error)
}

type ProcessClassifier interface {
	ProcessSignatures() []model.ProcessSignature
}

type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry { return &Registry{adapters: make(map[string]Adapter)} }

func (r *Registry) Register(adapter Adapter) error {
	if adapter == nil || adapter.ID() == "" {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "miner adapter ID is required"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[adapter.ID()]; exists {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "miner adapter is already registered"}
	}
	r.adapters[adapter.ID()] = adapter
	return nil
}

func (r *Registry) Get(id string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.adapters[id]
	return adapter, ok
}

func (r *Registry) ProcessSignatures() []model.ProcessSignature {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []model.ProcessSignature
	for _, adapter := range r.adapters {
		if classifier, ok := adapter.(ProcessClassifier); ok {
			result = append(result, classifier.ProcessSignatures()...)
		}
	}
	slices.SortFunc(result, func(a, b model.ProcessSignature) int {
		if a.Provider != b.Provider {
			return strings.Compare(a.Provider, b.Provider)
		}
		return strings.Compare(a.Executable, b.Executable)
	})
	return result
}

type Observation struct {
	Process   supervisor.Snapshot
	Telemetry *model.MinerTelemetry
	Warnings  []string
}

type Config struct {
	PollInterval     time.Duration
	StartupGrace     time.Duration
	StaleAfter       time.Duration
	Now              func() time.Time
	RefreshInventory func() model.Inventory
	ProcessObserver  *processobserve.Source
}

type Manager struct {
	mu             sync.Mutex
	bindingStartMu sync.Mutex
	pollWG         sync.WaitGroup
	supervisor     *supervisor.Supervisor
	registry       *Registry
	dataDir        string
	packages       PackageResolver
	inventory      model.Inventory
	config         Config
	executions     map[identity.ExecutionID]*minerExecution
}

type minerExecution struct {
	plan       model.ExecutionPlan
	prepared   Prepared
	warnings   []string
	telemetry  *model.MinerTelemetry
	pollCancel context.CancelFunc
	starting   bool
	invalid    bool
}

func New(supervisor *supervisor.Supervisor, registry *Registry, dataDir string, packageStore PackageResolver, inventory model.Inventory, config Config) *Manager {
	if config.PollInterval <= 0 {
		config.PollInterval = 3 * time.Second
	}
	if config.StartupGrace <= 0 {
		config.StartupGrace = 45 * time.Second
	}
	if config.StaleAfter <= 0 {
		config.StaleAfter = 10 * time.Second
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	manager := &Manager{supervisor: supervisor, registry: registry, dataDir: dataDir, packages: packageStore, inventory: inventory, config: config, executions: make(map[identity.ExecutionID]*minerExecution)}
	if supervisor != nil {
		supervisor.SetStartValidator(manager.acquireFinalStartValidation)
	}
	return manager
}

// SetInventory publishes fresh physical facts and immediately invalidates only
// exact Le0x-managed GPU executions whose immutable binding no longer matches.
// It never selects a replacement device or targets an unmanaged process.
func (m *Manager) SetInventory(inventory model.Inventory) {
	m.bindingStartMu.Lock()
	defer m.bindingStartMu.Unlock()
	m.setInventoryAndInvalidateLocked(inventory)
}

func (m *Manager) setInventoryAndInvalidateLocked(inventory model.Inventory) {
	type candidate struct {
		executionID identity.ExecutionID
		execution   *minerExecution
		plan        model.ExecutionPlan
	}
	m.mu.Lock()
	inventory = cloneInventory(inventory)
	m.inventory = inventory
	candidates := make([]candidate, 0)
	for executionID, execution := range m.executions {
		if execution.plan.Miner == nil || len(execution.plan.Ownership.DeviceIDs) == 0 {
			continue
		}
		candidates = append(candidates, candidate{executionID: executionID, execution: execution, plan: execution.plan})
	}
	m.mu.Unlock()

	invalid := make([]identity.ExecutionID, 0, len(candidates))
	for _, candidate := range candidates {
		if err := m.validatePlanAgainstInventory(candidate.plan, inventory); err != nil {
			m.mu.Lock()
			if m.executions[candidate.executionID] == candidate.execution {
				candidate.execution.invalid = true
				invalid = append(invalid, candidate.executionID)
			}
			m.mu.Unlock()
		}
	}
	for _, executionID := range invalid {
		// Stop uses the exact Controller-owned ExecutionID. If START has not yet
		// reached Supervisor this is harmless; final validation will still fail.
		_, _, _ = m.Stop(executionID)
	}
}

func (m *Manager) Start(ctx context.Context, plan model.ExecutionPlan) (Observation, string, error) {
	if plan.Miner == nil {
		snapshot, message, err := m.supervisor.Start(plan)
		return Observation{Process: snapshot}, message, err
	}
	plan.Miner = cloneMinerSpec(plan.Miner)
	inventory := m.inventoryForStart()
	m.mu.Lock()
	if existing := m.executions[plan.ExecutionID]; existing != nil {
		if !plansEqual(existing.plan, plan) {
			m.mu.Unlock()
			return Observation{}, "", farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "ExecutionID already has a different miner plan"}
		}
		if snapshot, ok := m.supervisor.Get(plan.ExecutionID); ok && (snapshot.State == model.ExecutionRunning || snapshot.State == model.ExecutionStarting || snapshot.State == model.ExecutionBackoff) {
			observation := m.observationLocked(existing, snapshot)
			m.mu.Unlock()
			return observation, "already running", nil
		}
		if existing.pollCancel != nil {
			existing.pollCancel()
		}
		if existing.prepared.Cleanup != nil {
			_ = existing.prepared.Cleanup()
		}
		delete(m.executions, plan.ExecutionID)
	}
	m.mu.Unlock()
	adapter, ok := m.registry.Get(plan.Miner.AdapterID)
	if !ok {
		return Observation{}, "", farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "unknown miner adapter"}
	}
	if err := validateResourcePlan(plan, adapter.Capabilities(), inventory); err != nil {
		return Observation{}, "", err
	}
	warnings, err := adapter.Validate(*plan.Miner, inventory)
	if err != nil {
		return Observation{}, "", err
	}
	preparePlan := plan
	preparePlan.Miner = cloneMinerSpec(plan.Miner)
	prepared, err := adapter.Prepare(ctx, PrepareRequest{Plan: preparePlan, AgentDataDir: m.dataDir, Packages: m.packages, Inventory: inventory})
	if err != nil {
		return Observation{}, "", err
	}
	if prepared.Telemetry == nil {
		if prepared.Cleanup != nil {
			_ = prepared.Cleanup()
		}
		return Observation{}, "", farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "miner adapter returned no telemetry source"}
	}
	// Adapter preparation may resolve executable details, but it cannot alter or
	// discard Controller ownership identity and resource claims.
	prepared.Plan.ExecutionID = plan.ExecutionID
	prepared.Plan.Ownership = plan.Ownership
	prepared.Plan.Ownership.DeviceIDs = append([]identity.DeviceID(nil), plan.Ownership.DeviceIDs...)
	prepared.Plan.HostID = plan.HostID
	prepared.Plan.DeviceIDs = append([]identity.DeviceID(nil), plan.DeviceIDs...)
	prepared.Plan.Miner = cloneMinerSpec(plan.Miner)
	entry := &minerExecution{plan: plan, prepared: prepared, warnings: warnings, telemetry: &model.MinerTelemetry{AdapterID: plan.Miner.AdapterID, Health: model.MinerHealthStarting}, starting: true}
	m.mu.Lock()
	m.executions[plan.ExecutionID] = entry
	m.mu.Unlock()
	snapshot, message, err := m.supervisor.Start(prepared.Plan)
	if err != nil {
		m.mu.Lock()
		if m.executions[plan.ExecutionID] == entry {
			delete(m.executions, plan.ExecutionID)
		}
		m.mu.Unlock()
		cleanupPrepared(entry)
		return Observation{Process: snapshot}, "", err
	}
	m.mu.Lock()
	if entry.invalid {
		entry.starting = false
		m.mu.Unlock()
		_, _, _ = m.Stop(plan.ExecutionID)
		return Observation{Process: snapshot}, "", farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: "GPU binding was invalidated during process start"}
	}
	pollCtx, cancel := context.WithCancel(context.Background())
	entry.pollCancel = cancel
	entry.starting = false
	observation := m.observationLocked(entry, snapshot)
	m.mu.Unlock()
	m.pollWG.Add(1)
	go func() {
		defer m.pollWG.Done()
		m.poll(pollCtx, plan.ExecutionID, entry)
	}()
	return observation, message, nil
}

func validateResourcePlan(plan model.ExecutionPlan, capabilities Capabilities, inventory model.Inventory) error {
	if err := plan.Ownership.Validate(); err != nil || plan.HostID != inventory.Host.HostID || plan.HostID != plan.Ownership.HostID || !slices.Equal(plan.DeviceIDs, plan.Ownership.DeviceIDs) {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "miner plan ownership or Host inventory does not agree"}
	}
	claimCPU, devices := plan.Ownership.CPU, plan.Ownership.DeviceIDs
	if claimCPU && len(devices) != 0 {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "combined CPU and GPU execution is not supported"}
	}
	if claimCPU && !capabilities.CPU {
		return farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: "miner adapter does not support CPU execution"}
	}
	if claimCPU {
		if inventory.CPU.Threads == 0 {
			return farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: "fresh Agent inventory does not report logical CPU capacity"}
		}
		if plan.Miner.CPUThreads != nil && *plan.Miner.CPUThreads > inventory.CPU.Threads {
			return farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: "requested CPU threads exceed fresh Agent capacity"}
		}
	}
	if len(devices) != 0 && !capabilities.GPU {
		return farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: "miner adapter does not support GPU execution"}
	}
	if len(devices) > 1 && !capabilities.MultiGPUSingleProcess {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "miner adapter does not support one process claiming multiple GPUs"}
	}
	if !claimCPU && (plan.Miner.CPUThreads != nil || plan.Miner.HugePages != nil || plan.Miner.MSR != nil) {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "CPU tuning is invalid for a GPU-only execution"}
	}
	if !slices.Equal(plan.Miner.GPUDeviceIDs, devices) {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "miner GPU DeviceIDs do not match ownership"}
	}
	if len(devices) != 0 {
		if err := gpuresource.ValidateCurrent(devices, plan.Miner.GPUAssignments, inventory, capabilities.GPUVendors); err != nil {
			return err
		}
	} else if len(plan.Miner.GPUAssignments) != 0 {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "CPU execution contains GPU assignments"}
	}
	return nil
}

func (m *Manager) inventoryForStart() model.Inventory {
	if m.config.RefreshInventory != nil {
		inventory := m.config.RefreshInventory()
		m.SetInventory(inventory)
		return inventory
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneInventory(m.inventory)
}

// acquireFinalStartValidation serializes fresh binding validation with
// inventory publication until the Supervisor has attempted cmd.Start. It is
// called for every actual process creation after Adapter.Prepare.
func (m *Manager) acquireFinalStartValidation(plan model.ExecutionPlan) (func(), error) {
	m.bindingStartMu.Lock()
	inventory := cloneInventory(m.inventory)
	if m.config.RefreshInventory != nil {
		inventory = m.config.RefreshInventory()
		m.setInventoryAndInvalidateLocked(inventory)
	}
	err := m.validatePlanAgainstInventory(plan, inventory)
	if err == nil {
		var processes []model.UnmanagedProcessObservation
		processes, err = m.observeUnmanaged(inventory)
		if err == nil {
			err = unmanagedConflict(plan, processes)
		}
	}
	if err != nil {
		m.bindingStartMu.Unlock()
		return nil, err
	}
	return m.bindingStartMu.Unlock, nil
}

// ObserveUnmanaged returns fresh observe-only process facts. It never mutates
// Supervisor ownership or process state.
func (m *Manager) ObserveUnmanaged() ([]model.UnmanagedProcessObservation, error) {
	m.bindingStartMu.Lock()
	defer m.bindingStartMu.Unlock()
	m.mu.Lock()
	inventory := cloneInventory(m.inventory)
	m.mu.Unlock()
	return m.observeUnmanaged(inventory)
}

func (m *Manager) observeUnmanaged(inventory model.Inventory) ([]model.UnmanagedProcessObservation, error) {
	if m.config.ProcessObserver == nil {
		return []model.UnmanagedProcessObservation{}, nil
	}
	managed := make([]processobserve.ManagedInstance, 0)
	for _, item := range m.supervisor.List() {
		if item.PID > 0 && item.ProcessInstance != "" {
			managed = append(managed, processobserve.ManagedInstance{PID: item.PID, ProcessInstance: item.ProcessInstance})
		}
	}
	processes, err := m.config.ProcessObserver.Observe(inventory, m.registry.ProcessSignatures(), managed)
	if err != nil {
		return nil, farmerr.Error{Code: farmerr.UNMANAGED_OBSERVATION_FAILED, HumanMessage: "fresh unmanaged process observation failed", Details: map[string]string{"reason": err.Error()}}
	}
	return processes, nil
}

func unmanagedConflict(plan model.ExecutionPlan, processes []model.UnmanagedProcessObservation) error {
	for _, process := range processes {
		overlaps := process.CPU && plan.Ownership.CPU
		for _, wanted := range plan.Ownership.DeviceIDs {
			if slices.Contains(process.DeviceIDs, wanted) {
				overlaps = true
				break
			}
		}
		ambiguous := process.ResourceScope == model.ProcessResourcesUnknown &&
			((plan.Ownership.CPU && process.CPURelevant) || (len(plan.Ownership.DeviceIDs) != 0 && process.GPURelevant))
		if !overlaps && !ambiguous {
			continue
		}
		details := map[string]string{"pid": strconv.Itoa(process.PID), "executable": process.Executable, "resource_scope": string(process.ResourceScope)}
		if len(process.Evidence) != 0 {
			details["evidence_kind"] = process.Evidence[0].Kind
			details["evidence_provider"] = process.Evidence[0].Provider
		}
		return farmerr.Error{Code: farmerr.UNMANAGED_PROCESS_CONFLICT, HumanMessage: "unmanaged mining process conflicts with requested resources", Details: details, SuggestedFix: "Stop the unmanaged process manually, then refresh observations before retrying."}
	}
	return nil
}

func (m *Manager) validatePlanAgainstInventory(plan model.ExecutionPlan, inventory model.Inventory) error {
	if plan.Miner == nil {
		return nil
	}
	adapter, ok := m.registry.Get(plan.Miner.AdapterID)
	if !ok {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "unknown miner adapter during process start"}
	}
	if err := validateResourcePlan(plan, adapter.Capabilities(), inventory); err != nil {
		return err
	}
	_, err := adapter.Validate(*plan.Miner, inventory)
	return err
}

func cloneInventory(value model.Inventory) model.Inventory {
	value.GPUs = append([]model.GPU(nil), value.GPUs...)
	return value
}

func cloneMinerSpec(value *model.MinerSpec) *model.MinerSpec {
	if value == nil {
		return nil
	}
	result := *value
	result.GPUDeviceIDs = append([]identity.DeviceID(nil), value.GPUDeviceIDs...)
	result.GPUAssignments = append([]model.GPUAssignment(nil), value.GPUAssignments...)
	result.Options = maps.Clone(value.Options)
	if value.Endpoint != nil {
		endpoint := *value.Endpoint
		result.Endpoint = &endpoint
	}
	return &result
}

func (m *Manager) Stop(id identity.ExecutionID) (Observation, string, error) {
	snapshot, message, err := m.supervisor.Stop(id)
	m.mu.Lock()
	entry := m.executions[id]
	if entry != nil {
		if entry.pollCancel != nil {
			entry.pollCancel()
			entry.pollCancel = nil
		}
		if !entry.starting {
			cleanupPrepared(entry)
		}
	}
	observation := m.observationLocked(entry, snapshot)
	m.mu.Unlock()
	return observation, message, err
}

func (m *Manager) Restart(id identity.ExecutionID) (Observation, string, error) {
	m.mu.Lock()
	entry := m.executions[id]
	m.mu.Unlock()
	snapshot, message, err := m.supervisor.Restart(id)
	m.mu.Lock()
	entry = m.executions[id]
	observation := m.observationLocked(entry, snapshot)
	m.mu.Unlock()
	return observation, message, err
}

func cleanupPrepared(entry *minerExecution) {
	if entry != nil && entry.prepared.Cleanup != nil {
		_ = entry.prepared.Cleanup()
		entry.prepared.Cleanup = nil
	}
}

func (m *Manager) List() []Observation {
	snapshots := m.supervisor.List()
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Observation, 0, len(snapshots))
	for _, snapshot := range snapshots {
		result = append(result, m.observationLocked(m.executions[snapshot.ExecutionID], snapshot))
	}
	return result
}

// OverallStatus aggregates only active real MINING executions. Diagnostic
// workloads and historical stopped executions never affect Agent status.
func (m *Manager) OverallStatus() model.AgentState {
	snapshots := m.supervisor.List()
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]overallStatusItem, 0, len(snapshots))
	for _, snapshot := range snapshots {
		entry := m.executions[snapshot.ExecutionID]
		if entry == nil || entry.plan.Miner == nil {
			continue
		}
		observation := m.observationLocked(entry, snapshot)
		items = append(items, overallStatusItem{mode: entry.plan.Miner.Mode, process: snapshot, telemetry: observation.Telemetry})
	}
	return aggregateOverallStatus(items)
}

type overallStatusItem struct {
	mode      model.MinerMode
	process   supervisor.Snapshot
	telemetry *model.MinerTelemetry
}

// aggregateOverallStatus uses safety precedence ERROR > DEGRADED > STARTING >
// MINING > IDLE. Stopped/stopping and non-MINING executions do not participate.
func aggregateOverallStatus(items []overallStatusItem) model.AgentState {
	result := model.AgentStateIdle
	for _, item := range items {
		if item.mode != model.MinerModeMining || item.process.State == model.ExecutionStopped || item.process.State == model.ExecutionStopping {
			continue
		}
		candidate := model.AgentStateStarting
		if item.telemetry != nil {
			switch item.telemetry.Health {
			case model.MinerHealthError:
				candidate = model.AgentStateError
			case model.MinerHealthDegraded:
				candidate = model.AgentStateDegraded
			case model.MinerHealthMining:
				candidate = model.AgentStateMining
			}
		}
		if agentStatePriority(candidate) > agentStatePriority(result) {
			result = candidate
		}
	}
	return result
}

func agentStatePriority(state model.AgentState) int {
	switch state {
	case model.AgentStateError:
		return 4
	case model.AgentStateDegraded:
		return 3
	case model.AgentStateStarting:
		return 2
	case model.AgentStateMining:
		return 1
	default:
		return 0
	}
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	for _, entry := range m.executions {
		if entry.pollCancel != nil {
			entry.pollCancel()
		}
	}
	m.mu.Unlock()
	err := m.supervisor.Shutdown(ctx)
	m.pollWG.Wait()
	m.mu.Lock()
	for _, entry := range m.executions {
		if entry.prepared.Cleanup != nil {
			err = errors.Join(err, entry.prepared.Cleanup())
			entry.prepared.Cleanup = nil
		}
	}
	m.mu.Unlock()
	return err
}

func (m *Manager) poll(ctx context.Context, id identity.ExecutionID, entry *minerExecution) {
	poll := func() {
		telemetry, err := entry.prepared.Telemetry.Poll(ctx)
		now := m.config.Now().UTC()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.executions[id] != entry {
			return
		}
		if err != nil {
			message := telemetryErrorMessage(err)
			if entry.telemetry == nil {
				entry.telemetry = &model.MinerTelemetry{AdapterID: entry.plan.Miner.AdapterID, Health: model.MinerHealthStarting, Message: message}
			} else {
				entry.telemetry.Age = now.Sub(entry.telemetry.CollectedAt)
				entry.telemetry.Message = message
			}
			return
		}
		telemetry.CollectedAt = now
		telemetry.Age = 0
		entry.telemetry = telemetry
	}
	poll()
	ticker := time.NewTicker(m.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}

func telemetryErrorMessage(err error) string {
	var typed farmerr.Error
	if errors.As(err, &typed) && typed.HumanMessage != "" {
		return typed.HumanMessage
	}
	return "miner telemetry unavailable"
}

func (m *Manager) observationLocked(entry *minerExecution, process supervisor.Snapshot) Observation {
	if entry == nil {
		return Observation{Process: process}
	}
	if process.State == model.ExecutionStopped {
		return Observation{Process: process, Warnings: append([]string(nil), entry.warnings...)}
	}
	var telemetry *model.MinerTelemetry
	if entry.telemetry != nil {
		copy := *entry.telemetry
		if !copy.CollectedAt.IsZero() {
			copy.Age = m.config.Now().UTC().Sub(copy.CollectedAt)
		}
		telemetry = &copy
	}
	telemetry = Evaluate(entry.plan.Miner.Mode, process, telemetry, m.config.Now().UTC(), m.config.StartupGrace, m.config.StaleAfter)
	return Observation{Process: process, Telemetry: telemetry, Warnings: append([]string(nil), entry.warnings...)}
}

func Evaluate(mode model.MinerMode, process supervisor.Snapshot, telemetry *model.MinerTelemetry, now time.Time, startupGrace, staleAfter time.Duration) *model.MinerTelemetry {
	if telemetry == nil {
		telemetry = &model.MinerTelemetry{}
	}
	if process.State == model.ExecutionFailed {
		telemetry.Health, telemetry.ErrorCode, telemetry.Message = model.MinerHealthError, farmerr.PROCESS_CRASHED, process.LastError
		return telemetry
	}
	if mode == model.MinerModeMining && (process.State == model.ExecutionBackoff || process.State == model.ExecutionCrashed) {
		telemetry.Health, telemetry.ErrorCode = model.MinerHealthDegraded, farmerr.PROCESS_CRASHED
		return telemetry
	}
	if process.State != model.ExecutionRunning {
		telemetry.Health = model.MinerHealthStarting
		return telemetry
	}
	withinGrace := !process.StartedAt.IsZero() && now.Sub(process.StartedAt) < startupGrace
	if telemetry.CollectedAt.IsZero() || telemetry.Age > staleAfter {
		if withinGrace {
			telemetry.Health = model.MinerHealthStarting
		} else {
			telemetry.Health, telemetry.ErrorCode = model.MinerHealthDegraded, farmerr.RPC_UNREACHABLE
		}
		return telemetry
	}
	hashrate := float64(0)
	if telemetry.HashrateShortHPS != nil {
		hashrate = *telemetry.HashrateShortHPS
	}
	if mode == model.MinerModeMining {
		if excessiveRejects(telemetry) {
			telemetry.Health, telemetry.ErrorCode = model.MinerHealthDegraded, farmerr.TOO_MANY_REJECTS
		} else if hashrate > 0 && telemetry.PoolConnected != nil && *telemetry.PoolConnected {
			telemetry.Health, telemetry.ErrorCode = model.MinerHealthMining, ""
		} else if withinGrace {
			telemetry.Health = model.MinerHealthStarting
		} else if hashrate <= 0 {
			telemetry.Health, telemetry.ErrorCode = model.MinerHealthDegraded, farmerr.ZERO_HASHRATE
		} else {
			telemetry.Health, telemetry.ErrorCode = model.MinerHealthDegraded, farmerr.POOL_UNREACHABLE
		}
	} else {
		telemetry.ErrorCode = ""
		telemetry.Health = model.MinerHealthHealthy
		if hashrate <= 0 && !withinGrace {
			telemetry.Health, telemetry.ErrorCode = model.MinerHealthDegraded, farmerr.ZERO_HASHRATE
		}
	}
	return telemetry
}

// excessiveRejects waits for a useful sample and then treats more than 20%
// rejected shares as degraded. Adapters only provide normalized counters.
func excessiveRejects(telemetry *model.MinerTelemetry) bool {
	if telemetry.AcceptedShares == nil || telemetry.RejectedShares == nil {
		return false
	}
	total := *telemetry.AcceptedShares + *telemetry.RejectedShares
	return total >= 10 && float64(*telemetry.RejectedShares)/float64(total) > 0.20
}

func plansEqual(a, b model.ExecutionPlan) bool { return reflect.DeepEqual(a, b) }

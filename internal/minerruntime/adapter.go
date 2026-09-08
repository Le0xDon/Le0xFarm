// Package minerruntime connects miner adapters to the generic process supervisor.
package minerruntime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/packages"
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

type Observation struct {
	Process   supervisor.Snapshot
	Telemetry *model.MinerTelemetry
	Warnings  []string
}

type Config struct {
	PollInterval time.Duration
	StartupGrace time.Duration
	StaleAfter   time.Duration
	Now          func() time.Time
}

type Manager struct {
	mu         sync.Mutex
	pollWG     sync.WaitGroup
	supervisor *supervisor.Supervisor
	registry   *Registry
	dataDir    string
	packages   PackageResolver
	inventory  model.Inventory
	config     Config
	executions map[identity.ExecutionID]*minerExecution
}

type minerExecution struct {
	plan       model.ExecutionPlan
	prepared   Prepared
	warnings   []string
	telemetry  *model.MinerTelemetry
	pollCancel context.CancelFunc
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
	return &Manager{supervisor: supervisor, registry: registry, dataDir: dataDir, packages: packageStore, inventory: inventory, config: config, executions: make(map[identity.ExecutionID]*minerExecution)}
}

func (m *Manager) Start(ctx context.Context, plan model.ExecutionPlan) (Observation, string, error) {
	if plan.Miner == nil {
		snapshot, message, err := m.supervisor.Start(plan)
		return Observation{Process: snapshot}, message, err
	}
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
	warnings, err := adapter.Validate(*plan.Miner, m.inventory)
	if err != nil {
		return Observation{}, "", err
	}
	prepared, err := adapter.Prepare(ctx, PrepareRequest{Plan: plan, AgentDataDir: m.dataDir, Packages: m.packages, Inventory: m.inventory})
	if err != nil {
		return Observation{}, "", err
	}
	if prepared.Telemetry == nil {
		if prepared.Cleanup != nil {
			_ = prepared.Cleanup()
		}
		return Observation{}, "", farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "miner adapter returned no telemetry source"}
	}
	snapshot, message, err := m.supervisor.Start(prepared.Plan)
	if err != nil {
		if prepared.Cleanup != nil {
			_ = prepared.Cleanup()
		}
		return Observation{Process: snapshot}, "", err
	}
	pollCtx, cancel := context.WithCancel(context.Background())
	entry := &minerExecution{plan: plan, prepared: prepared, warnings: warnings, telemetry: &model.MinerTelemetry{AdapterID: plan.Miner.AdapterID, Health: model.MinerHealthStarting}, pollCancel: cancel}
	m.mu.Lock()
	if old := m.executions[plan.ExecutionID]; old != nil {
		if old.pollCancel != nil {
			old.pollCancel()
		}
		if old.prepared.Cleanup != nil {
			_ = old.prepared.Cleanup()
		}
	}
	m.executions[plan.ExecutionID] = entry
	observation := m.observationLocked(entry, snapshot)
	m.mu.Unlock()
	m.pollWG.Add(1)
	go func() {
		defer m.pollWG.Done()
		m.poll(pollCtx, plan.ExecutionID, entry)
	}()
	return observation, message, nil
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
		if entry.prepared.Cleanup != nil {
			_ = entry.prepared.Cleanup()
			entry.prepared.Cleanup = nil
		}
	}
	observation := m.observationLocked(entry, snapshot)
	m.mu.Unlock()
	return observation, message, err
}

func (m *Manager) Restart(id identity.ExecutionID) (Observation, string, error) {
	snapshot, message, err := m.supervisor.Restart(id)
	m.mu.Lock()
	entry := m.executions[id]
	observation := m.observationLocked(entry, snapshot)
	m.mu.Unlock()
	return observation, message, err
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

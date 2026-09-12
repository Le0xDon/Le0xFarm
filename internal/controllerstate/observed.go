// Package controllerstate owns ephemeral, epoch-scoped Controller observations.
package controllerstate

import (
	"slices"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

type ConnectionEpoch uint64

// DefaultFreshnessTimeout allows several missed normal 10-second Agent
// heartbeats before current-epoch execution or inventory facts become
// unusable for reconciliation. It is intentionally conservative to avoid
// flapping on a short scheduling or network delay.
const DefaultFreshnessTimeout = 45 * time.Second

type HostObservation struct {
	HostID               identity.HostID
	AgentID              identity.AgentID
	ConnectionEpoch      ConnectionEpoch
	Connected            bool
	Ready                bool
	Fresh                bool
	RefreshedAt          time.Time
	InventoryFresh       bool
	InventoryRefreshedAt time.Time
	Inventory            model.Inventory
	ProcessesFresh       bool
	ProcessesRefreshedAt time.Time
	UnmanagedProcesses   []model.UnmanagedProcessObservation
	AgentState           model.AgentState
	Executions           []model.ExecutionObservation
	// RuntimeSequence is the greatest accepted current-epoch runtime action
	// watermark represented by Executions.
	RuntimeSequence uint64
	// Revision is a diagnostic watermark that changes whenever current-epoch
	// reconciliation facts are refreshed. Coordinators compare the relevant
	// facts themselves so an equivalent heartbeat refresh cannot starve work.
	Revision uint64
}

type entry struct {
	observation      HostObservation
	inventoryCurrent bool
	statusCurrent    bool
	processesCurrent bool
	freshAt          time.Time
	inventoryFreshAt time.Time
	processesFreshAt time.Time
	runtimeSequence  uint64
}

type Store struct {
	mu               sync.RWMutex
	hosts            map[identity.HostID]*entry
	now              func() time.Time
	freshnessTimeout time.Duration
}

type StoreOptions struct {
	Now              func() time.Time
	FreshnessTimeout time.Duration
}

func New() *Store { return NewWithOptions(StoreOptions{}) }

func NewWithOptions(options StoreOptions) *Store {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.FreshnessTimeout <= 0 {
		options.FreshnessTimeout = DefaultFreshnessTimeout
	}
	return &Store{hosts: make(map[identity.HostID]*entry), now: options.Now, freshnessTimeout: options.FreshnessTimeout}
}

func (store *Store) Connect(agentID identity.AgentID, hostID identity.HostID, epoch ConnectionEpoch) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	if current := store.hosts[hostID]; current != nil && current.observation.ConnectionEpoch >= epoch {
		return false
	}
	store.hosts[hostID] = &entry{observation: HostObservation{HostID: hostID, AgentID: agentID, ConnectionEpoch: epoch, Connected: true, Revision: 1}}
	return true
}

func (store *Store) SetExecutions(hostID identity.HostID, epoch ConnectionEpoch, executions []model.ExecutionObservation, refreshedAt time.Time, runtimeSequence uint64) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	item := store.current(hostID, epoch)
	if item == nil || runtimeSequence < item.runtimeSequence {
		return false
	}
	item.observation.Executions = cloneExecutions(executions)
	item.observation.Fresh = true
	item.observation.RefreshedAt = refreshedAt.UTC()
	item.observation.RuntimeSequence = runtimeSequence
	item.observation.Revision++
	item.freshAt = store.now()
	item.runtimeSequence = runtimeSequence
	return true
}

func (store *Store) SetInventory(hostID identity.HostID, epoch ConnectionEpoch, inventory model.Inventory) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	item := store.current(hostID, epoch)
	if item == nil {
		return false
	}
	item.observation.Inventory = cloneInventory(inventory)
	item.inventoryCurrent = true
	item.observation.InventoryFresh = true
	now := store.now()
	item.observation.InventoryRefreshedAt = now.UTC()
	item.inventoryFreshAt = now
	item.observation.Revision++
	return true
}

func (store *Store) SetUnmanagedProcesses(hostID identity.HostID, epoch ConnectionEpoch, processes []model.UnmanagedProcessObservation, refreshedAt time.Time) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	item := store.current(hostID, epoch)
	if item == nil {
		return false
	}
	item.observation.UnmanagedProcesses = cloneProcesses(processes)
	item.processesCurrent = true
	item.observation.ProcessesFresh = true
	item.observation.ProcessesRefreshedAt = refreshedAt.UTC()
	item.processesFreshAt = store.now()
	item.observation.Revision++
	return true
}

func (store *Store) SetAgentState(hostID identity.HostID, epoch ConnectionEpoch, state model.AgentState) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	item := store.current(hostID, epoch)
	if item == nil {
		return false
	}
	item.observation.AgentState = state
	item.statusCurrent = true
	item.observation.Revision++
	return true
}

func (store *Store) MarkReady(hostID identity.HostID, epoch ConnectionEpoch) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	item := store.current(hostID, epoch)
	if item == nil || item.observation.Ready || !item.observation.Fresh || !item.inventoryCurrent || !item.statusCurrent || !item.processesCurrent {
		return false
	}
	item.observation.Ready = true
	item.observation.Revision++
	return true
}

// RequireFreshBootstrap invalidates all START-authorizing observations without
// disconnecting the current session. Maintenance exit uses it before normal
// reconciliation may resume.
func (store *Store) RequireFreshBootstrap(hostID identity.HostID, epoch ConnectionEpoch) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	item := store.current(hostID, epoch)
	if item == nil {
		return false
	}
	item.observation.Ready = false
	item.observation.Fresh = false
	item.observation.InventoryFresh = false
	item.observation.ProcessesFresh = false
	item.freshAt = time.Time{}
	item.inventoryFreshAt = time.Time{}
	item.processesFreshAt = time.Time{}
	item.inventoryCurrent = false
	item.processesCurrent = false
	item.statusCurrent = false
	item.observation.Revision++
	return true
}

func (store *Store) UpsertExecution(hostID identity.HostID, epoch ConnectionEpoch, execution model.ExecutionObservation, refreshedAt time.Time, runtimeSequence uint64) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	item := store.current(hostID, epoch)
	if item == nil || !item.observation.Fresh || runtimeSequence < item.runtimeSequence {
		return false
	}
	found := false
	for i := range item.observation.Executions {
		if item.observation.Executions[i].ExecutionID == execution.ExecutionID {
			item.observation.Executions[i] = cloneExecution(execution)
			found = true
			break
		}
	}
	if !found {
		item.observation.Executions = append(item.observation.Executions, cloneExecution(execution))
	}
	slices.SortFunc(item.observation.Executions, func(a, b model.ExecutionObservation) int {
		return compare(a.ExecutionID.String(), b.ExecutionID.String())
	})
	item.observation.RefreshedAt = refreshedAt.UTC()
	item.observation.RuntimeSequence = runtimeSequence
	item.observation.Revision++
	item.freshAt = store.now()
	item.runtimeSequence = runtimeSequence
	return true
}

func (store *Store) Disconnect(hostID identity.HostID, epoch ConnectionEpoch) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	item := store.current(hostID, epoch)
	if item == nil {
		return false
	}
	item.observation.Connected = false
	item.observation.Ready = false
	item.observation.Fresh = false
	item.freshAt = time.Time{}
	item.inventoryCurrent = false
	item.inventoryFreshAt = time.Time{}
	item.observation.InventoryFresh = false
	item.processesCurrent = false
	item.processesFreshAt = time.Time{}
	item.observation.ProcessesFresh = false
	item.statusCurrent = false
	item.observation.Revision++
	return true
}

func (store *Store) Get(hostID identity.HostID) (HostObservation, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	item := store.hosts[hostID]
	if item == nil {
		return HostObservation{}, false
	}
	store.refreshFreshness(item)
	return store.cloneCurrent(item), true
}

func (store *Store) refreshFreshness(item *entry) {
	if item.observation.Fresh && (item.freshAt.IsZero() || store.now().Sub(item.freshAt) > store.freshnessTimeout) {
		item.observation.Fresh = false
		item.observation.Revision++
	}
	if item.observation.InventoryFresh && (item.inventoryFreshAt.IsZero() || store.now().Sub(item.inventoryFreshAt) > store.freshnessTimeout) {
		item.observation.InventoryFresh = false
		item.observation.Revision++
	}
	if item.observation.ProcessesFresh && (item.processesFreshAt.IsZero() || store.now().Sub(item.processesFreshAt) > store.freshnessTimeout) {
		item.observation.ProcessesFresh = false
		item.observation.Revision++
	}
}

func (store *Store) cloneCurrent(item *entry) HostObservation {
	result := cloneObservation(item.observation)
	if !item.freshAt.IsZero() {
		ageObservedTelemetry(&result, store.now().Sub(item.freshAt))
	}
	return result
}

// List returns bounded copies of all Hosts known during this Controller
// lifetime. Observations remain epoch-scoped and are never persisted.
func (store *Store) List() []HostObservation {
	store.mu.Lock()
	defer store.mu.Unlock()
	result := make([]HostObservation, 0, len(store.hosts))
	for _, item := range store.hosts {
		store.refreshFreshness(item)
		result = append(result, store.cloneCurrent(item))
	}
	slices.SortFunc(result, func(a, b HostObservation) int {
		return compare(a.HostID.String(), b.HostID.String())
	})
	return result
}

func ageObservedTelemetry(observation *HostObservation, elapsed time.Duration) {
	if elapsed <= 0 {
		return
	}
	staleWorking := false
	for index := range observation.Executions {
		execution := &observation.Executions[index]
		telemetry := execution.MinerTelemetry
		evidence := execution.UsefulWork
		if evidence == nil {
			continue
		}
		if telemetry != nil {
			telemetry.Age += elapsed
		}
		evidence.Age += elapsed
		if evidence.Availability == model.TelemetryAvailable && evidence.FreshFor > 0 && evidence.Age > evidence.FreshFor {
			wasConfirmedActive := execution.Status == model.ExecutionRunning && evidence.UsefulWork == model.UsefulWorkConfirmed
			evidence.Availability = model.TelemetryStale
			evidence.ReasonCode = farmerr.TELEMETRY_STALE
			if telemetry != nil {
				telemetry.Health = model.MinerHealthDegraded
				telemetry.ErrorCode = farmerr.TELEMETRY_STALE
			}
			staleWorking = staleWorking || wasConfirmedActive
		}
	}
	if staleWorking && observation.AgentState == model.AgentStateMining {
		observation.AgentState = model.AgentStateDegraded
	}
}

func (store *Store) current(hostID identity.HostID, epoch ConnectionEpoch) *entry {
	item := store.hosts[hostID]
	if item == nil || item.observation.ConnectionEpoch != epoch || !item.observation.Connected {
		return nil
	}
	return item
}

func cloneObservation(value HostObservation) HostObservation {
	value.Inventory = cloneInventory(value.Inventory)
	value.Executions = cloneExecutions(value.Executions)
	value.UnmanagedProcesses = cloneProcesses(value.UnmanagedProcesses)
	return value
}

func cloneProcesses(values []model.UnmanagedProcessObservation) []model.UnmanagedProcessObservation {
	result := make([]model.UnmanagedProcessObservation, len(values))
	for i, value := range values {
		value.DeviceIDs = append([]identity.DeviceID(nil), value.DeviceIDs...)
		value.Evidence = append([]model.ProcessEvidence(nil), value.Evidence...)
		result[i] = value
	}
	return result
}

func cloneInventory(value model.Inventory) model.Inventory {
	value.GPUs = append([]model.GPU(nil), value.GPUs...)
	return value
}

func cloneExecutions(values []model.ExecutionObservation) []model.ExecutionObservation {
	result := make([]model.ExecutionObservation, len(values))
	for i, value := range values {
		result[i] = cloneExecution(value)
	}
	return result
}

func cloneExecution(value model.ExecutionObservation) model.ExecutionObservation {
	value.Warnings = append([]string(nil), value.Warnings...)
	if value.Ownership != nil {
		copy := *value.Ownership
		copy.DeviceIDs = append([]identity.DeviceID(nil), value.Ownership.DeviceIDs...)
		value.Ownership = &copy
	}
	if value.ExitCode != nil {
		copy := *value.ExitCode
		value.ExitCode = &copy
	}
	if value.MinerTelemetry != nil {
		copy := *value.MinerTelemetry
		copy.PerDevice = append([]model.DeviceHashrate(nil), value.MinerTelemetry.PerDevice...)
		copy.HashrateShortHPS = copyFloat64(value.MinerTelemetry.HashrateShortHPS)
		copy.HashrateMediumHPS = copyFloat64(value.MinerTelemetry.HashrateMediumHPS)
		copy.HashrateLongHPS = copyFloat64(value.MinerTelemetry.HashrateLongHPS)
		copy.HighestHashrateHPS = copyFloat64(value.MinerTelemetry.HighestHashrateHPS)
		copy.AcceptedShares = copyUint64(value.MinerTelemetry.AcceptedShares)
		copy.RejectedShares = copyUint64(value.MinerTelemetry.RejectedShares)
		copy.StaleShares = copyUint64(value.MinerTelemetry.StaleShares)
		copy.TotalResults = copyUint64(value.MinerTelemetry.TotalResults)
		copy.PoolConnected = copyBool(value.MinerTelemetry.PoolConnected)
		copy.PoolLatencyMS = copyUint32(value.MinerTelemetry.PoolLatencyMS)
		copy.HugePagesAvailable = copyBool(value.MinerTelemetry.HugePagesAvailable)
		copy.HugePagesPercent = copyFloat64(value.MinerTelemetry.HugePagesPercent)
		copy.MSRAvailable = copyBool(value.MinerTelemetry.MSRAvailable)
		if value.MinerTelemetry.UsefulWork != nil {
			evidence := *value.MinerTelemetry.UsefulWork
			evidence.RuntimeHealthy = copyBool(value.MinerTelemetry.UsefulWork.RuntimeHealthy)
			evidence.JobPresent = copyBool(value.MinerTelemetry.UsefulWork.JobPresent)
			evidence.AcceptedWork = copyUint64(value.MinerTelemetry.UsefulWork.AcceptedWork)
			evidence.RejectedWork = copyUint64(value.MinerTelemetry.UsefulWork.RejectedWork)
			evidence.StaleWork = copyUint64(value.MinerTelemetry.UsefulWork.StaleWork)
			evidence.EndpointVisible = copyBool(value.MinerTelemetry.UsefulWork.EndpointVisible)
			evidence.Metrics = append([]model.WorkMetric(nil), value.MinerTelemetry.UsefulWork.Metrics...)
			if value.MinerTelemetry.UsefulWork.LastUsefulWorkAt != nil {
				last := *value.MinerTelemetry.UsefulWork.LastUsefulWorkAt
				evidence.LastUsefulWorkAt = &last
			}
			copy.UsefulWork = &evidence
		}
		value.MinerTelemetry = &copy
	}
	value.UsefulWork = copyUsefulWork(value.UsefulWork)
	return value
}

func copyUsefulWork(value *model.UsefulWorkEvidence) *model.UsefulWorkEvidence {
	if value == nil {
		return nil
	}
	copy := *value
	copy.RuntimeHealthy = copyBool(value.RuntimeHealthy)
	copy.JobPresent = copyBool(value.JobPresent)
	copy.AcceptedWork = copyUint64(value.AcceptedWork)
	copy.RejectedWork = copyUint64(value.RejectedWork)
	copy.StaleWork = copyUint64(value.StaleWork)
	copy.EndpointVisible = copyBool(value.EndpointVisible)
	copy.Metrics = append([]model.WorkMetric(nil), value.Metrics...)
	if value.LastUsefulWorkAt != nil {
		last := *value.LastUsefulWorkAt
		copy.LastUsefulWorkAt = &last
	}
	return &copy
}

func copyBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyUint32(value *uint32) *uint32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func compare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

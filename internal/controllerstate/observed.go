// Package controllerstate owns ephemeral, epoch-scoped Controller observations.
package controllerstate

import (
	"slices"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

type ConnectionEpoch uint64

// DefaultFreshnessTimeout allows several missed normal 10-second Agent
// heartbeats before current-epoch execution facts become unusable for
// reconciliation. It is intentionally conservative to avoid flapping on a
// short scheduling or network delay.
const DefaultFreshnessTimeout = 45 * time.Second

type HostObservation struct {
	HostID          identity.HostID
	AgentID         identity.AgentID
	ConnectionEpoch ConnectionEpoch
	Connected       bool
	Ready           bool
	Fresh           bool
	RefreshedAt     time.Time
	Inventory       model.Inventory
	AgentState      model.AgentState
	Executions      []model.ExecutionObservation
	// RuntimeSequence is the greatest accepted current-epoch runtime action
	// watermark represented by Executions.
	RuntimeSequence uint64
	// Revision changes whenever current-epoch reconciliation facts change.
	// Coordinators use it to reject a plan whose observation changed between
	// planning and network dispatch.
	Revision uint64
}

type entry struct {
	observation      HostObservation
	inventoryCurrent bool
	statusCurrent    bool
	freshAt          time.Time
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
	if item == nil || !item.observation.Fresh || !item.inventoryCurrent || !item.statusCurrent {
		return false
	}
	item.observation.Ready = true
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
	if item.observation.Fresh && (item.freshAt.IsZero() || store.now().Sub(item.freshAt) > store.freshnessTimeout) {
		item.observation.Fresh = false
		item.observation.Revision++
	}
	return cloneObservation(item.observation), true
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
	return value
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
		value.MinerTelemetry = &copy
	}
	return value
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

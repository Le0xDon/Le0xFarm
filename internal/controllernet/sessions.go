package controllernet

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/wiremap"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
)

type SessionInfo struct {
	AgentID         identity.AgentID
	HostID          identity.HostID
	ConnectionEpoch controllerstate.ConnectionEpoch
	Authenticated   bool
	Ready           bool
}

type RuntimeKind string

const (
	RuntimeStart RuntimeKind = "START"
	RuntimeStop  RuntimeKind = "STOP"
)

type RuntimeRequest struct {
	Kind              RuntimeKind
	WorkloadID        identity.WorkloadID
	DesiredGeneration uint64
	ExecutionID       identity.ExecutionID
	ResolvedHash      string
	Plan              model.ExecutionPlan
	// DispatchSequence is assigned by the live Controller session and is never
	// persisted or sent on the wire. It orders fresh execution snapshots after
	// uncertain runtime actions within one ConnectionEpoch.
	DispatchSequence uint64
}

type SessionHandler interface {
	SessionConnected(SessionInfo)
	SessionDisconnected(SessionInfo)
	ExecutionsObserved(SessionInfo, []model.ExecutionObservation, uint64)
	InventoryObserved(SessionInfo, model.Inventory)
	StatusObserved(SessionInfo, model.AgentState)
	SessionReady(SessionInfo)
	RuntimeResult(SessionInfo, RuntimeRequest, *model.ExecutionObservation, *farmerr.Error)
}

type ProcessSessionHandler interface {
	ProcessesObserved(SessionInfo, []model.UnmanagedProcessObservation)
}

type liveSession struct {
	info            SessionInfo
	stream          le0xv1.AgentControl_ConnectServer
	sendGate        chan struct{}
	revoked         chan struct{}
	finished        chan struct{}
	revokeOnce      sync.Once
	pendingMu       sync.Mutex
	pending         map[string]pendingCommand
	runtimeSequence uint64
}

func (s *Server) SetSessionHandler(handler SessionHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = handler
}

func (s *Server) Session(hostID identity.HostID) (SessionInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.sessions[hostID]
	if live == nil || live.isRevoked() {
		return SessionInfo{}, false
	}
	return live.info, true
}

func (s *Server) SendRuntime(ctx context.Context, info SessionInfo, request RuntimeRequest) (uint64, error) {
	s.mu.Lock()
	live := s.sessions[info.HostID]
	if live == nil || live.isRevoked() || live.info.ConnectionEpoch != info.ConnectionEpoch || live.info.AgentID != info.AgentID || !live.info.Authenticated || !live.info.Ready {
		s.mu.Unlock()
		return 0, farmerr.Error{Code: farmerr.SERVICE_NOT_READY, HumanMessage: "Agent session is not ready for runtime actions"}
	}
	s.mu.Unlock()
	reject := func(err error) (uint64, error) {
		// The coordinator treats a synchronous command-boundary failure as an
		// uncertain delivery and waits for a new epoch. Make that assumption true
		// even for local validation failures after selecting a READY session.
		live.revoke()
		return 0, err
	}
	if request.ExecutionID.Validate() != nil || request.WorkloadID.Validate() != nil || request.DesiredGeneration < 1 || request.ResolvedHash == "" {
		return reject(farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid runtime request identity"})
	}
	command := &le0xv1.CommandEnvelope{CommandId: commandID()}
	switch request.Kind {
	case RuntimeStart:
		plan, err := wiremap.ExecutionPlan(request.Plan)
		if err != nil {
			return reject(err)
		}
		command.Command = &le0xv1.CommandEnvelope_StartExecution{StartExecution: &le0xv1.StartExecution{Plan: plan}}
	case RuntimeStop:
		command.Command = &le0xv1.CommandEnvelope_StopExecution{StopExecution: &le0xv1.StopExecution{ExecutionId: request.ExecutionID.String()}}
	default:
		return reject(farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "unsupported runtime request"})
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sendCtx, cancel := context.WithTimeout(ctx, s.config.RuntimeActionTimeout)
	defer cancel()
	sequence, err := live.send(sendCtx, command, pendingCommand{kind: commandKind(command), request: &request})
	if err != nil {
		live.revoke()
		return 0, err
	}
	go s.expireRuntimeResult(live, command.CommandId, sequence)
	return sequence, nil
}

func (s *Server) SendMaintenanceHold(ctx context.Context, info SessionInfo, active bool, revision uint64) error {
	s.mu.Lock()
	live := s.sessions[info.HostID]
	if live == nil || live.isRevoked() || live.info.ConnectionEpoch != info.ConnectionEpoch || live.info.AgentID != info.AgentID || !live.info.Authenticated {
		s.mu.Unlock()
		return farmerr.Error{Code: farmerr.SERVICE_NOT_READY, HumanMessage: "Agent session is unavailable for Maintenance Hold"}
	}
	s.mu.Unlock()
	if revision < 1 {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "Maintenance Hold revision is invalid"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sendCtx, cancel := context.WithTimeout(ctx, s.config.RuntimeActionTimeout)
	defer cancel()
	done := make(chan error, 1)
	command := &le0xv1.CommandEnvelope{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_SetMaintenanceHold{SetMaintenanceHold: &le0xv1.SetMaintenanceHold{Active: active, Revision: revision}}}
	if _, err := live.send(sendCtx, command, pendingCommand{kind: commandKind(command), holdActive: active, holdRevision: revision, done: done}); err != nil {
		live.revoke()
		return err
	}
	select {
	case err := <-done:
		return err
	case <-sendCtx.Done():
		live.revoke()
		return sendCtx.Err()
	case <-live.revoked:
		return farmerr.Error{Code: farmerr.SERVICE_NOT_READY, HumanMessage: "Agent session closed before Maintenance Hold confirmation"}
	case <-live.stream.Context().Done():
		return live.stream.Context().Err()
	}
}

func (s *Server) expireRuntimeResult(live *liveSession, commandID string, sequence uint64) {
	timer := time.NewTimer(s.config.RuntimeActionTimeout)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-live.revoked:
		return
	case <-live.stream.Context().Done():
		return
	}
	live.pendingMu.Lock()
	pending, exists := live.pending[commandID]
	if exists && pending.request != nil && pending.runtimeSequence == sequence {
		delete(live.pending, commandID)
	} else {
		exists = false
	}
	live.pendingMu.Unlock()
	if !exists || !s.sessionCurrent(live) {
		return
	}
	typed := farmerr.Error{Code: farmerr.INTERNAL_ERROR, HumanMessage: "runtime result deadline exceeded; delivery outcome is uncertain"}
	if handler := s.handlerSnapshot(); handler != nil {
		handler.RuntimeResult(s.sessionInfo(live), *pending.request, nil, &typed)
	}
}

func (live *liveSession) send(ctx context.Context, command *le0xv1.CommandEnvelope, pending pendingCommand) (uint64, error) {
	if command == nil {
		return 0, errors.New("command is required")
	}
	select {
	case live.sendGate <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-live.revoked:
		return 0, farmerr.Error{Code: farmerr.SERVICE_NOT_READY, HumanMessage: "Agent session was replaced or closed"}
	case <-live.stream.Context().Done():
		return 0, live.stream.Context().Err()
	}
	releaseGate := func() { <-live.sendGate }
	if err := ctx.Err(); err != nil {
		releaseGate()
		return 0, err
	}
	if live.isRevoked() {
		releaseGate()
		return 0, farmerr.Error{Code: farmerr.SERVICE_NOT_READY, HumanMessage: "Agent session was replaced or closed"}
	}
	if err := live.stream.Context().Err(); err != nil {
		releaseGate()
		return 0, err
	}

	live.pendingMu.Lock()
	if pending.request != nil {
		live.runtimeSequence++
		pending.runtimeSequence = live.runtimeSequence
		requestCopy := *pending.request
		requestCopy.DispatchSequence = pending.runtimeSequence
		pending.request = &requestCopy
	} else if pending.kind == "GET_EXECUTIONS" {
		pending.runtimeSequence = live.runtimeSequence
	}
	live.pending[command.CommandId] = pending
	live.pendingMu.Unlock()

	result := make(chan error, 1)
	go func() {
		err := live.stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Command{Command: command}})
		<-live.sendGate
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			live.removePending(command.CommandId)
			return 0, err
		}
		return pending.runtimeSequence, nil
	case <-ctx.Done():
		live.removePending(command.CommandId)
		live.revoke()
		return 0, ctx.Err()
	case <-live.revoked:
		live.removePending(command.CommandId)
		return 0, farmerr.Error{Code: farmerr.SERVICE_NOT_READY, HumanMessage: "Agent session was replaced or closed"}
	case <-live.stream.Context().Done():
		live.removePending(command.CommandId)
		return 0, live.stream.Context().Err()
	}
}

func (live *liveSession) removePending(commandID string) {
	live.pendingMu.Lock()
	delete(live.pending, commandID)
	live.pendingMu.Unlock()
}

func (live *liveSession) revoke() { live.revokeOnce.Do(func() { close(live.revoked) }) }

func (live *liveSession) isRevoked() bool {
	select {
	case <-live.revoked:
		return true
	default:
		return false
	}
}

func (s *Server) handlerSnapshot() SessionHandler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handler
}

func (s *Server) sessionCurrent(live *liveSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[live.info.HostID] == live && !live.isRevoked()
}

func (s *Server) sessionInfo(live *liveSession) SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return live.info
}

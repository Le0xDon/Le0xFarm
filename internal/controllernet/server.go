// Package controllernet owns authenticated Agent sessions and their safe command boundary.
package controllernet

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerpki"
	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/controllertrust"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/inventory"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/protocol"
	"github.com/le0xdon/le0xfarm/internal/wiremap"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Config struct {
	ControllerID         identity.ControllerID
	FarmID               identity.FarmID
	ListenAddress        string
	InsecureDev          bool
	ProtocolVersion      uint32
	SchemaVersion        uint32
	Inventory            inventory.Source
	Output               *log.Logger
	ShutdownGracePeriod  time.Duration
	RuntimeActionTimeout time.Duration
	Trust                *controllertrust.Store
	Pairing              *PairingWindow
	PKI                  *controllerpki.PKI
	// RuntimeCommands are development/test commands sent through the authenticated
	// AgentControl stream. Production desired-state control is a later milestone.
	RuntimeCommands []*le0xv1.CommandEnvelope
	RuntimeTarget   identity.AgentID
	Maintenance     MaintenanceProvider
}

type MaintenanceProvider interface {
	GetMaintenanceHold(context.Context, identity.HostID) (farmmodel.MaintenanceHold, bool, error)
}

type Server struct {
	le0xv1.UnimplementedAgentControlServer
	controllerID       identity.ControllerID
	farmID             identity.FarmID
	config             Config
	grpcServer         *grpc.Server
	mu                 sync.Mutex
	sessionLifecycleMu sync.Mutex
	maintenanceMu      sync.Mutex
	maintenanceLocks   map[identity.HostID]*sync.Mutex
	connections        int
	nextEpoch          controllerstate.ConnectionEpoch
	sessions           map[identity.HostID]*liveSession
	handler            SessionHandler
}

type pendingCommand struct {
	kind            string
	nonce           []byte
	request         *RuntimeRequest
	runtimeSequence uint64
	holdActive      bool
	holdRevision    uint64
	done            chan error
}

func New(config Config) (*Server, error) {
	if !config.InsecureDev && config.PKI == nil {
		return nil, farmerr.Error{Code: farmerr.TLS_CREDENTIALS_REQUIRED, HumanMessage: "Controller TLS credentials are required"}
	}
	if config.ProtocolVersion == 0 {
		config.ProtocolVersion = uint32(protocol.CurrentProtocolVersion)
	}
	if config.SchemaVersion == 0 {
		config.SchemaVersion = uint32(protocol.CurrentSchemaVersion)
	}
	if config.Output == nil {
		config.Output = log.Default()
	}
	if config.ShutdownGracePeriod <= 0 {
		config.ShutdownGracePeriod = 3 * time.Second
	}
	if config.RuntimeActionTimeout <= 0 {
		config.RuntimeActionTimeout = 10 * time.Second
	}
	if err := config.ControllerID.Validate(); err != nil {
		return nil, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "ControllerID is required", Details: map[string]string{"reason": err.Error()}}
	}
	if err := config.FarmID.Validate(); err != nil {
		return nil, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "FarmID is required", Details: map[string]string{"reason": err.Error()}}
	}
	if config.Trust == nil {
		return nil, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "Controller trust store is required"}
	}
	if len(config.RuntimeCommands) > 0 {
		if err := config.RuntimeTarget.Validate(); err != nil {
			return nil, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "runtime commands require a target AgentID"}
		}
	}
	return &Server{controllerID: config.ControllerID, farmID: config.FarmID, config: config, sessions: make(map[identity.HostID]*liveSession), maintenanceLocks: make(map[identity.HostID]*sync.Mutex)}, nil
}

func (s *Server) IDs() (identity.ControllerID, identity.FarmID) { return s.controllerID, s.farmID }

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	options := []grpc.ServerOption{}
	if !s.config.InsecureDev {
		options = append(options, grpc.Creds(credentials.NewTLS(s.config.PKI.TLSConfig(s.config.Pairing != nil))))
	}
	s.grpcServer = grpc.NewServer(options...)
	le0xv1.RegisterAgentControlServer(s.grpcServer, s)
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.grpcServer.Serve(listener) }()
	select {
	case err := <-serveErr:
		if err == grpc.ErrServerStopped {
			return nil
		}
		return err
	case <-ctx.Done():
		// GracefulStop waits for active streams, so bound it and force-close if needed.
		gracefulDone := make(chan struct{})
		go func() { s.grpcServer.GracefulStop(); close(gracefulDone) }()
		timer := time.NewTimer(s.config.ShutdownGracePeriod)
		select {
		case <-gracefulDone:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			s.grpcServer.Stop()
			<-gracefulDone
		}
		err := <-serveErr
		if err == grpc.ErrServerStopped {
			return nil
		}
		return err
	}
}

func (s *Server) Connect(stream le0xv1.AgentControl_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return statusError(farmerr.CONFIG_CONFLICT, "first Agent message must be AgentHello")
	}
	if hello.ProtocolVersion != s.config.ProtocolVersion {
		s.log("Rejected agent: protocol mismatch")
		return statusError(farmerr.PROTOCOL_VERSION_MISMATCH, "protocol version mismatch")
	}
	if hello.SchemaVersion != s.config.SchemaVersion {
		s.log("Rejected agent: schema mismatch")
		return statusError(farmerr.SCHEMA_VERSION_MISMATCH, "schema version mismatch")
	}
	agentID, err := identity.ParseAgentID(hello.AgentId)
	if err != nil {
		s.log("Rejected agent: invalid AgentID")
		return statusError(farmerr.CONFIG_CONFLICT, "invalid AgentID")
	}
	hostID, err := identity.ParseHostID(hello.HostId)
	if err != nil {
		s.log("Rejected agent: invalid HostID")
		return statusError(farmerr.CONFIG_CONFLICT, "invalid HostID")
	}
	if !s.config.InsecureDev {
		cert, hasCert := clientCertificate(stream.Context())
		if !hasCert {
			if hello.EnrollmentToken == "" {
				return statusError(farmerr.TLS_CREDENTIALS_REQUIRED, "Agent client certificate or enrollment credentials are required")
			}
			if len(hello.CertificateRequestDer) == 0 {
				return statusError(farmerr.TLS_CREDENTIALS_REQUIRED, "Agent certificate request is required")
			}
			if s.config.Pairing == nil {
				return statusError(farmerr.PAIRING_REQUIRED, "Controller pairing window is not enabled")
			}
			csr, parseErr := x509.ParseCertificateRequest(hello.CertificateRequestDer)
			if parseErr != nil || csr.CheckSignature() != nil {
				return statusError(farmerr.TLS_IDENTITY_MISMATCH, "invalid Agent certificate request")
			}
			if !csrMatches(csr, s.farmID, agentID, hostID) {
				return statusError(farmerr.TLS_IDENTITY_MISMATCH, "Agent CSR identity does not match AgentHello")
			}
			var issued []byte
			err := s.config.Pairing.Use(hello.EnrollmentToken, func() error {
				var issueErr error
				issued, issueErr = s.config.PKI.IssueAgent(hello.CertificateRequestDer, agentID, hostID, time.Now())
				if issueErr != nil {
					return issueErr
				}
				if record, ok := s.config.Trust.Find(agentID); ok {
					if record.HostID != hostID {
						return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "AgentID is paired to another HostID"}
					}
					return nil
				}
				return s.config.Trust.Pair(agentID, hostID)
			})
			if err != nil {
				if code, ok := farmerr.CodeOf(err); ok {
					return statusError(code, err.Error())
				}
				return err
			}
			active, revision, err := s.maintenanceState(stream.Context(), hostID)
			if err != nil {
				return statusError(farmerr.SERVICE_NOT_READY, "Maintenance Hold state is unavailable")
			}
			return stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Hello{Hello: &le0xv1.ControllerHello{ProtocolVersion: s.config.ProtocolVersion, SchemaVersion: s.config.SchemaVersion, ControllerId: s.controllerID.String(), FarmId: s.farmID.String(), AgentCertificateDer: issued, FarmCaCertificateDer: s.config.PKI.CACertificateDER(), MaintenanceHoldActive: active, MaintenanceHoldRevision: revision}}})
		}
		if err := controllerpki.VerifyAgentCertificate(cert, s.config.PKI.CA, s.farmID, agentID, hostID, time.Now()); err != nil {
			if code, ok := farmerr.CodeOf(err); ok {
				return statusError(code, err.Error())
			}
			return err
		}
		if record, ok := s.config.Trust.Find(agentID); !ok {
			return statusError(farmerr.PAIRING_REQUIRED, "Agent certificate is valid but Agent is not paired")
		} else if record.HostID != hostID {
			return statusError(farmerr.TLS_IDENTITY_MISMATCH, "Agent trust record does not match certificate")
		}
	} else {
		if record, paired := s.config.Trust.Find(agentID); paired {
			if record.HostID != hostID {
				return statusError(farmerr.CONFIG_CONFLICT, "AgentID is paired to another HostID")
			}
		} else {
			if hello.EnrollmentToken == "" {
				return statusError(farmerr.PAIRING_REQUIRED, "Agent is not paired; enrollment token required")
			}
			if s.config.Pairing == nil {
				return statusError(farmerr.PAIRING_REQUIRED, "Controller pairing window is not enabled")
			}
			if err := s.config.Pairing.Use(hello.EnrollmentToken, func() error { return s.config.Trust.Pair(agentID, hostID) }); err != nil {
				if code, ok := farmerr.CodeOf(err); ok {
					return statusError(code, err.Error())
				}
				return err
			}
		}
	}
	s.sessionLifecycleMu.Lock()
	s.mu.Lock()
	previous := s.sessions[hostID]
	s.mu.Unlock()
	if previous != nil {
		previous.revoke()
		timer := time.NewTimer(s.config.RuntimeActionTimeout)
		select {
		case <-previous.finished:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			s.sessionLifecycleMu.Unlock()
			return statusError(farmerr.SERVICE_NOT_READY, "previous Agent session did not close within the replacement bound")
		case <-stream.Context().Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			s.sessionLifecycleMu.Unlock()
			return stream.Context().Err()
		}
	}
	maintenanceLock := s.maintenanceLock(hostID)
	maintenanceLock.Lock()
	holdActive, holdRevision, err := s.maintenanceState(stream.Context(), hostID)
	if err != nil {
		maintenanceLock.Unlock()
		s.sessionLifecycleMu.Unlock()
		return statusError(farmerr.SERVICE_NOT_READY, "Maintenance Hold state is unavailable")
	}
	s.mu.Lock()
	s.connections++
	s.nextEpoch++
	live := &liveSession{info: SessionInfo{AgentID: agentID, HostID: hostID, ConnectionEpoch: s.nextEpoch, Authenticated: true}, stream: stream, sendGate: make(chan struct{}, 1), revoked: make(chan struct{}), finished: make(chan struct{}), pending: make(map[string]pendingCommand)}
	s.sessions[hostID] = live
	handler := s.handler
	s.mu.Unlock()
	s.sessionLifecycleMu.Unlock()
	defer func() {
		live.revoke()
		s.mu.Lock()
		if s.connections > 0 {
			s.connections--
		}
		if s.sessions[hostID] == live {
			delete(s.sessions, hostID)
		}
		handler := s.handler
		s.mu.Unlock()
		if handler != nil {
			handler.SessionDisconnected(s.sessionInfo(live))
		}
		close(live.finished)
	}()
	s.log("Agent connected:")
	s.log("AgentID: %s", agentID)
	s.log("HostID: %s", hostID)
	s.log("Hostname: %s", hello.Hostname)
	s.log("Protocol: %d", hello.ProtocolVersion)
	s.log("Schema: %d", hello.SchemaVersion)
	if err := stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Hello{Hello: &le0xv1.ControllerHello{ProtocolVersion: s.config.ProtocolVersion, SchemaVersion: s.config.SchemaVersion, ControllerId: s.controllerID.String(), FarmId: s.farmID.String(), MaintenanceHoldActive: holdActive, MaintenanceHoldRevision: holdRevision}}}); err != nil {
		maintenanceLock.Unlock()
		return err
	}
	maintenanceLock.Unlock()
	if handler != nil {
		handler.SessionConnected(live.info)
	}
	queue := func(command *le0xv1.CommandEnvelope) error {
		kind := commandKind(command)
		sendCtx, cancel := context.WithTimeout(stream.Context(), s.config.RuntimeActionTimeout)
		defer cancel()
		_, err := live.send(sendCtx, command, pendingCommand{kind: kind, nonce: append([]byte(nil), command.GetPing().GetNonce()...)})
		return err
	}
	queueIfAbsent := func(command *le0xv1.CommandEnvelope) error {
		kind := commandKind(command)
		live.pendingMu.Lock()
		for _, pending := range live.pending {
			if pending.kind == kind {
				live.pendingMu.Unlock()
				return nil
			}
		}
		live.pendingMu.Unlock()
		return queue(command)
	}
	if err := queue(&le0xv1.CommandEnvelope{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_GetExecutions{GetExecutions: &le0xv1.GetExecutions{}}}); err != nil {
		return err
	}
	type receivedMessage struct {
		message *le0xv1.AgentMessage
		err     error
	}
	received := make(chan receivedMessage, 1)
	go func() {
		for {
			message, err := stream.Recv()
			select {
			case received <- receivedMessage{message: message, err: err}:
			case <-live.revoked:
				return
			case <-stream.Context().Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var gotExecutions, gotInventory, gotProcesses, gotStatus, bootstrapRequested, devSent, ready bool
	for {
		var message *le0xv1.AgentMessage
		var err error
		select {
		case item := <-received:
			message, err = item.message, item.err
		case <-live.revoked:
			return statusError(farmerr.SERVICE_NOT_READY, "Agent session was replaced or closed")
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
		if err != nil {
			if err != io.EOF {
				s.log("Agent disconnected: %v", err)
			}
			return err
		}
		if !s.sessionCurrent(live) {
			return statusError(farmerr.SERVICE_NOT_READY, "Agent session was replaced")
		}
		if heartbeat := message.GetHeartbeat(); heartbeat != nil {
			s.log("Heartbeat: %s", heartbeat.Timestamp.AsTime().Format(time.RFC3339))
			// Runtime state can change locally after the bootstrap snapshot (for
			// example, the bounded watchdog can reach terminal FAILED). Refresh
			// executions, inventory, and aggregate status on every normal heartbeat so the
			// level-triggered Controller eventually observes that fact without a
			// reconnect. At most one request of each kind may be pending.
			if ready && s.sessionCurrent(live) {
				if err := queueIfAbsent(&le0xv1.CommandEnvelope{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_GetExecutions{GetExecutions: &le0xv1.GetExecutions{}}}); err != nil {
					return err
				}
				if err := queueIfAbsent(&le0xv1.CommandEnvelope{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_GetStatus{GetStatus: &le0xv1.GetStatus{}}}); err != nil {
					return err
				}
				if err := queueIfAbsent(&le0xv1.CommandEnvelope{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_GetInventory{GetInventory: &le0xv1.GetInventory{}}}); err != nil {
					return err
				}
				if err := queueIfAbsent(&le0xv1.CommandEnvelope{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_GetUnmanagedProcesses{GetUnmanagedProcesses: &le0xv1.GetUnmanagedProcesses{}}}); err != nil {
					return err
				}
			}
			continue
		}
		result := message.GetCommandResult()
		if result == nil {
			continue
		}
		live.pendingMu.Lock()
		pendingResult, ok := live.pending[result.CommandId]
		if !ok {
			live.pendingMu.Unlock()
			s.log("CommandResult rejected: unknown command_id %q", result.CommandId)
			continue
		}
		delete(live.pending, result.CommandId)
		live.pendingMu.Unlock()
		if result.GetError() != nil {
			s.log("%s: ERROR %s", pendingResult.kind, result.GetError().Code)
			typed := &farmerr.Error{Code: farmerr.Code(result.GetError().Code), HumanMessage: result.GetError().HumanMessage, Details: result.GetError().Details, SuggestedFix: result.GetError().SuggestedFix, LogsRef: result.GetError().LogsRef}
			if pendingResult.done != nil {
				pendingResult.done <- typed
			}
			if pendingResult.request != nil {
				if handler := s.handlerSnapshot(); handler != nil {
					handler.RuntimeResult(s.sessionInfo(live), *pendingResult.request, nil, typed)
				}
			}
			continue
		}
		switch pendingResult.kind {
		case "Ping":
			if !pingResultValid(result, pendingResult.nonce) {
				s.log("PING: invalid/rejected result (nonce mismatch)")
			} else {
				s.log("PING: OK")
			}
		case "GetStatus":
			if result.GetStatus() == nil {
				s.log("STATUS: invalid result")
			} else {
				state := model.AgentState(result.GetStatus().AgentState)
				if !validAgentState(state) {
					s.log("STATUS: invalid result")
					break
				}
				gotStatus = true
				s.log("STATUS: %s", state)
				if handler := s.handlerSnapshot(); handler != nil {
					handler.StatusObserved(s.sessionInfo(live), state)
				}
			}
		case "GetInventory":
			if result.GetInventory() == nil {
				s.log("INVENTORY: invalid result")
			} else if parsed, err := wiremap.ParseInventory(result.GetInventory(), hostID); err != nil {
				s.log("INVENTORY: rejected result (%v)", err)
			} else {
				gotInventory = true
				s.logInventory(result.GetInventory())
				if handler := s.handlerSnapshot(); handler != nil {
					handler.InventoryObserved(s.sessionInfo(live), parsed)
				}
			}
		case "START_EXECUTION", "STOP_EXECUTION", "RESTART_EXECUTION":
			if result.GetExecution() == nil || result.GetExecution().Execution == nil {
				s.log("%s: invalid result", strings.ToUpper(pendingResult.kind))
				if pendingResult.request != nil {
					typed := farmerr.Error{Code: farmerr.INTERNAL_ERROR, HumanMessage: "malformed runtime execution result"}
					if handler := s.handlerSnapshot(); handler != nil {
						handler.RuntimeResult(s.sessionInfo(live), *pendingResult.request, nil, &typed)
					}
				}
			} else {
				s.logExecution(pendingResult.kind, result.GetExecution().Execution, result.GetExecution().Message)
				if pendingResult.request != nil {
					parsed, parseErr := wiremap.ParseExecution(result.GetExecution().Execution, hostID)
					if handler := s.handlerSnapshot(); handler != nil {
						if parseErr != nil {
							typed := farmerr.Error{Code: farmerr.INTERNAL_ERROR, HumanMessage: "invalid runtime execution result"}
							handler.RuntimeResult(s.sessionInfo(live), *pendingResult.request, nil, &typed)
						} else {
							handler.RuntimeResult(s.sessionInfo(live), *pendingResult.request, &parsed, nil)
						}
					}
				}
			}
		case "GET_EXECUTIONS":
			if result.GetExecutions() == nil {
				s.log("GET_EXECUTIONS: invalid result")
			} else if parsed, err := wiremap.ParseExecutions(result.GetExecutions(), hostID); err != nil {
				s.log("GET_EXECUTIONS: rejected result (%v)", err)
			} else {
				gotExecutions = true
				s.log("EXECUTIONS: %d", len(result.GetExecutions().Executions))
				for _, execution := range result.GetExecutions().Executions {
					s.logExecution("Execution", execution, "")
				}
				if handler := s.handlerSnapshot(); handler != nil {
					handler.ExecutionsObserved(s.sessionInfo(live), parsed, pendingResult.runtimeSequence)
				}
				if !bootstrapRequested {
					bootstrapRequested = true
					if err := queue(&le0xv1.CommandEnvelope{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_GetStatus{GetStatus: &le0xv1.GetStatus{}}}); err != nil {
						return err
					}
					if err := queue(&le0xv1.CommandEnvelope{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_GetInventory{GetInventory: &le0xv1.GetInventory{}}}); err != nil {
						return err
					}
					if err := queue(&le0xv1.CommandEnvelope{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_GetUnmanagedProcesses{GetUnmanagedProcesses: &le0xv1.GetUnmanagedProcesses{}}}); err != nil {
						return err
					}
					if err := queue(&le0xv1.CommandEnvelope{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_Ping{Ping: &le0xv1.Ping{Nonce: nonce()}}}); err != nil {
						return err
					}
				}
			}
		case "GET_UNMANAGED_PROCESSES":
			if result.GetUnmanagedProcesses() == nil {
				s.log("GET_UNMANAGED_PROCESSES: invalid result")
			} else if parsed, err := wiremap.ParseUnmanagedProcesses(result.GetUnmanagedProcesses()); err != nil {
				s.log("GET_UNMANAGED_PROCESSES: rejected result (%v)", err)
			} else {
				gotProcesses = true
				s.log("UNMANAGED_PROCESSES: %d", len(parsed))
				if handler, ok := s.handlerSnapshot().(ProcessSessionHandler); ok {
					handler.ProcessesObserved(s.sessionInfo(live), parsed)
				}
			}
		case "SET_MAINTENANCE_HOLD":
			state := result.GetMaintenanceHold()
			if pendingResult.done == nil {
				break
			}
			if state == nil || state.Active != pendingResult.holdActive || state.Revision != pendingResult.holdRevision {
				pendingResult.done <- farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "Agent returned mismatched Maintenance Hold state"}
			} else {
				pendingResult.done <- nil
			}
		}
		if gotExecutions && gotInventory && gotProcesses && gotStatus && !ready && s.sessionCurrent(live) {
			s.mu.Lock()
			if s.sessions[hostID] == live {
				live.info.Ready = true
				ready = true
			}
			s.mu.Unlock()
			if handler := s.handlerSnapshot(); handler != nil {
				handler.SessionReady(s.sessionInfo(live))
			}
		}
		if ready && !devSent && agentID == s.config.RuntimeTarget {
			devSent = true
			for _, configured := range s.config.RuntimeCommands {
				if configured == nil {
					continue
				}
				command := proto.Clone(configured).(*le0xv1.CommandEnvelope)
				command.CommandId = commandID()
				if err := queue(command); err != nil {
					return err
				}
			}
		}
	}
}

func (s *Server) logExecution(kind string, execution *le0xv1.Execution, message string) {
	s.log("%s: %s state=%s pid=%d restart_count=%d message=%q last_error=%q", strings.ToUpper(kind), execution.ExecutionId, execution.State, execution.Pid, execution.RestartCount, message, execution.LastError)
	for _, warning := range execution.Warnings {
		s.log("EXECUTION WARNING: %s", warning)
	}
	if telemetry := execution.GetMinerTelemetry(); telemetry != nil {
		hashrate := "unavailable"
		if telemetry.HashrateShortHps != nil {
			hashrate = fmt.Sprintf("%.3f H/s", telemetry.GetHashrateShortHps())
		}
		hugePages, msr := "unreported", "unreported"
		if telemetry.HugePagesPercent != nil {
			hugePages = fmt.Sprintf("%.1f%%", telemetry.GetHugePagesPercent())
		}
		if telemetry.MsrAvailable != nil {
			msr = fmt.Sprintf("%t", telemetry.GetMsrAvailable())
		}
		s.log("MINER: adapter=%s version=%s algorithm=%s health=%s hashrate=%s age=%dms huge_pages=%s msr_available=%s error=%s", telemetry.AdapterId, telemetry.MinerVersion, telemetry.Algorithm, telemetry.Health, hashrate, telemetry.AgeMilliseconds, hugePages, msr, telemetry.ErrorCode)
	}
}

func pingResultValid(result *le0xv1.CommandResult, expected []byte) bool {
	return result != nil && result.GetPong() != nil && bytes.Equal(result.GetPong().GetNonce(), expected)
}

func validateInventoryWire(in *le0xv1.Inventory, expectedHost identity.HostID) error {
	host, err := identity.ParseHostID(in.HostId)
	if err != nil {
		return fmt.Errorf("invalid inventory HostID: %w", err)
	}
	if host != expectedHost {
		return fmt.Errorf("inventory HostID does not match AgentHello")
	}
	for _, gpu := range in.Gpus {
		if gpu == nil || gpu.DeviceId == "" {
			continue
		}
		if _, err := identity.ParseDeviceID(gpu.DeviceId); err != nil {
			return fmt.Errorf("invalid GPU DeviceID: %w", err)
		}
	}
	return nil
}

func validAgentState(state model.AgentState) bool {
	switch state {
	case model.AgentStateIdle, model.AgentStateStarting, model.AgentStateMining, model.AgentStateDegraded, model.AgentStateError, model.AgentStateMaintenance:
		return true
	default:
		return false
	}
}

func (s *Server) logInventory(in *le0xv1.Inventory) {
	s.log("Inventory:")
	s.log("OS: %s %s", in.OsId, in.OsVersion)
	s.log("CPU: %s", in.CpuModel)
	s.log("Cores: %d", in.CpuCores)
	s.log("Threads: %d", in.CpuThreads)
	s.log("Memory: %d bytes", in.MemoryTotalBytes)
	s.log("GPUs: %d", len(in.Gpus))
}
func commandKind(command *le0xv1.CommandEnvelope) string {
	switch {
	case command.GetPing() != nil:
		return "Ping"
	case command.GetGetStatus() != nil:
		return "GetStatus"
	case command.GetGetInventory() != nil:
		return "GetInventory"
	case command.GetGetUnmanagedProcesses() != nil:
		return "GET_UNMANAGED_PROCESSES"
	case command.GetSetMaintenanceHold() != nil:
		return "SET_MAINTENANCE_HOLD"
	case command.GetStartExecution() != nil:
		return "START_EXECUTION"
	case command.GetStopExecution() != nil:
		return "STOP_EXECUTION"
	case command.GetRestartExecution() != nil:
		return "RESTART_EXECUTION"
	case command.GetGetExecutions() != nil:
		return "GET_EXECUTIONS"
	default:
		return "Unknown"
	}
}

func (s *Server) maintenanceState(ctx context.Context, hostID identity.HostID) (bool, uint64, error) {
	if s.config.Maintenance == nil {
		return false, 0, nil
	}
	hold, exists, err := s.config.Maintenance.GetMaintenanceHold(ctx, hostID)
	if err != nil || !exists {
		return false, 0, err
	}
	return hold.Active, hold.Revision, nil
}

// AcquireMaintenanceAuthority serializes persistent Hold publication with the
// reconnect hello that establishes Agent-side process-creation authority.
func (s *Server) AcquireMaintenanceAuthority(hostID identity.HostID) func() {
	lock := s.maintenanceLock(hostID)
	lock.Lock()
	return lock.Unlock
}

func (s *Server) maintenanceLock(hostID identity.HostID) *sync.Mutex {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	lock := s.maintenanceLocks[hostID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.maintenanceLocks[hostID] = lock
	}
	return lock
}
func (s *Server) log(format string, args ...any) {
	if s.config.Output != nil {
		s.config.Output.Printf(format, args...)
	}
}
func commandID() string { var b [16]byte; _, _ = rand.Read(b[:]); return fmt.Sprintf("cmd_%x", b[:]) }
func nonce() []byte     { var b [16]byte; _, _ = rand.Read(b[:]); return b[:] }
func statusError(code farmerr.Code, message string) error {
	return status.Error(codes.InvalidArgument, fmt.Sprintf("%s: %s", code, message))
}

func clientCertificate(ctx context.Context) (*x509.Certificate, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return nil, false
	}
	return info.State.PeerCertificates[0], true
}
func csrMatches(csr *x509.CertificateRequest, farmID identity.FarmID, agentID identity.AgentID, hostID identity.HostID) bool {
	want := controllerpki.AgentURI(farmID, agentID, hostID).String()
	for _, u := range csr.URIs {
		if u.String() == want {
			return true
		}
	}
	return false
}

func (s *Server) ActiveConnections() int { s.mu.Lock(); defer s.mu.Unlock(); return s.connections }

var _ le0xv1.AgentControlServer = (*Server)(nil)

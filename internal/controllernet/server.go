// Package controllernet contains the minimal development Controller gRPC runtime.
package controllernet

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/inventory"
	"github.com/le0xdon/le0xfarm/internal/protocol"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Config struct {
	ListenAddress   string
	InsecureDev     bool
	ProtocolVersion uint32
	SchemaVersion   uint32
	Inventory       inventory.Source
	Output          *log.Logger
}

type Server struct {
	le0xv1.UnimplementedAgentControlServer
	controllerID identity.ControllerID
	farmID       identity.FarmID
	config       Config
	grpcServer   *grpc.Server
	mu           sync.Mutex
	connections  int
}

type pendingCommand struct {
	kind  string
	nonce []byte
}

func New(config Config) (*Server, error) {
	if !config.InsecureDev {
		return nil, farmerr.Error{Code: farmerr.PERMISSION_DENIED, HumanMessage: "Secure transport is not implemented yet; refusing plaintext listener", SuggestedFix: "Pass --insecure-dev only for development/test transport."}
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
	controllerID, err := identity.NewControllerID()
	if err != nil {
		return nil, err
	}
	farmID, err := identity.NewFarmID()
	if err != nil {
		return nil, err
	}
	return &Server{controllerID: controllerID, farmID: farmID, config: config}, nil
}

func (s *Server) IDs() (identity.ControllerID, identity.FarmID) { return s.controllerID, s.farmID }

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	s.grpcServer = grpc.NewServer()
	le0xv1.RegisterAgentControlServer(s.grpcServer, s)
	go func() { <-ctx.Done(); s.grpcServer.GracefulStop() }()
	err := s.grpcServer.Serve(listener)
	if err == grpc.ErrServerStopped {
		return nil
	}
	return err
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
	s.mu.Lock()
	s.connections++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.connections > 0 {
			s.connections--
		}
		s.mu.Unlock()
	}()
	s.log("Agent connected:")
	s.log("AgentID: %s", agentID)
	s.log("HostID: %s", hostID)
	s.log("Hostname: %s", hello.Hostname)
	s.log("Protocol: %d", hello.ProtocolVersion)
	s.log("Schema: %d", hello.SchemaVersion)
	if err := stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Hello{Hello: &le0xv1.ControllerHello{ProtocolVersion: s.config.ProtocolVersion, SchemaVersion: s.config.SchemaVersion, ControllerId: s.controllerID.String(), FarmId: s.farmID.String()}}}); err != nil {
		return err
	}
	commands := []*le0xv1.CommandEnvelope{{CommandId: commandID(), Command: &le0xv1.CommandEnvelope_Ping{Ping: &le0xv1.Ping{Nonce: nonce()}}}, {CommandId: commandID(), Command: &le0xv1.CommandEnvelope_GetStatus{GetStatus: &le0xv1.GetStatus{}}}, {CommandId: commandID(), Command: &le0xv1.CommandEnvelope_GetInventory{GetInventory: &le0xv1.GetInventory{}}}}
	pending := make(map[string]pendingCommand, len(commands))
	for _, command := range commands {
		kind := commandKind(command)
		pending[command.CommandId] = pendingCommand{kind: kind, nonce: append([]byte(nil), command.GetPing().GetNonce()...)}
		if err := stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Command{Command: command}}); err != nil {
			return err
		}
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				s.log("Agent disconnected: %v", err)
			}
			return err
		}
		if heartbeat := message.GetHeartbeat(); heartbeat != nil {
			s.log("Heartbeat: %s", heartbeat.Timestamp.AsTime().Format(time.RFC3339))
			continue
		}
		result := message.GetCommandResult()
		if result == nil {
			continue
		}
		pendingResult, ok := pending[result.CommandId]
		if !ok {
			s.log("CommandResult rejected: unknown command_id %q", result.CommandId)
			continue
		}
		delete(pending, result.CommandId)
		if result.GetError() != nil {
			s.log("%s: ERROR %s", pendingResult.kind, result.GetError().Code)
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
				s.log("STATUS: %s", result.GetStatus().AgentState)
			}
		case "GetInventory":
			if result.GetInventory() == nil {
				s.log("INVENTORY: invalid result")
			} else if err := validateInventoryWire(result.GetInventory(), hostID); err != nil {
				s.log("INVENTORY: rejected result (%v)", err)
			} else {
				s.logInventory(result.GetInventory())
			}
		}
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
	default:
		return "Unknown"
	}
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

func (s *Server) ActiveConnections() int { s.mu.Lock(); defer s.mu.Unlock(); return s.connections }

var _ le0xv1.AgentControlServer = (*Server)(nil)

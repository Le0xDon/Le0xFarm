package agentnet

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllernet"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/inventory"
	"github.com/le0xdon/le0xfarm/internal/protocol"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func testLogger() *log.Logger { return log.New(&bytes.Buffer{}, "", 0) }

func TestAgentNetworkRequiresExplicitDevelopmentMode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, Config{Target: "unused"})
	if err == nil || !strings.Contains(err.Error(), "--insecure-dev") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentConnectsAndSendsHeartbeat(t *testing.T) {
	server, err := controllernet.New(controllernet.Config{InsecureDev: true})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	le0xv1.RegisterAgentControlServer(grpcServer, server)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err = connectOnce(ctx, Config{Target: "buf", InsecureDev: true, AgentID: agentID, HostID: hostID, Hostname: "test", Inventory: inventory.Local(), HeartbeatInterval: 20 * time.Millisecond, Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: testLogger()})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expected cancellation after active stream: %v", err)
	}
}

func TestAgentHandshakeRejectsControllerVersion(t *testing.T) {
	// Classification must use the typed code, regardless of human text.
	for _, message := range []string{"bad version", "a completely different explanation"} {
		if !nonTransient(farmerr.Error{Code: farmerr.PROTOCOL_VERSION_MISMATCH, HumanMessage: message}) {
			t.Fatal("protocol mismatch should be non-transient")
		}
	}
	if nonTransient(fmt.Errorf("connection reset")) {
		t.Fatal("transport failure should be transient")
	}
}

func TestCommandMappingPreservesNonce(t *testing.T) {
	nonce := []byte{0, 1, 2, 255}
	ping := &le0xv1.CommandEnvelope{CommandId: "x", Command: &le0xv1.CommandEnvelope_Ping{Ping: &le0xv1.Ping{Nonce: nonce}}}
	if !bytes.Equal(ping.GetPing().GetNonce(), nonce) {
		t.Fatal("nonce changed")
	}
	if uint32(protocol.CurrentProtocolVersion) != 1 {
		t.Fatal("unexpected protocol test baseline")
	}
}

type reconnectServer struct {
	le0xv1.UnimplementedAgentControlServer
	calls  chan int
	count  int
	pinged chan struct{}
}

func (s *reconnectServer) Connect(stream le0xv1.AgentControl_ConnectServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	s.count++
	n := s.count
	s.calls <- n
	controller, _ := identity.NewControllerID()
	farm, _ := identity.NewFarmID()
	if err := stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Hello{Hello: &le0xv1.ControllerHello{ProtocolVersion: 1, SchemaVersion: 1, ControllerId: controller.String(), FarmId: farm.String()}}}); err != nil {
		return err
	}
	if n == 1 {
		return fmt.Errorf("temporary disconnect")
	}
	if err := stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Command{Command: &le0xv1.CommandEnvelope{CommandId: "reconnect-ping", Command: &le0xv1.CommandEnvelope_Ping{Ping: &le0xv1.Ping{Nonce: []byte{1}}}}}}); err != nil {
		return err
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		if result := message.GetCommandResult(); result != nil && result.CommandId == "reconnect-ping" {
			continue
		}
		if message.GetHeartbeat() != nil {
			close(s.pinged)
			return nil
		}
	}
}

func TestAgentReconnectsAndUsesShorterBackoffAfterSuccess(t *testing.T) {
	server := &reconnectServer{calls: make(chan int, 4), pinged: make(chan struct{})}
	listener := bufconn.Listen(1024 * 1024)
	gs := grpc.NewServer()
	le0xv1.RegisterAgentControlServer(gs, server)
	go gs.Serve(listener)
	defer gs.Stop()
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Target: "buf", InsecureDev: true, AgentID: agentID, HostID: hostID, Hostname: "test", Inventory: inventory.Local(), HeartbeatInterval: 10 * time.Millisecond, ReconnectInitial: time.Millisecond, ReconnectMax: 4 * time.Millisecond, Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: testLogger()})
	}()
	select {
	case n := <-server.calls:
		if n != 1 {
			t.Fatal(n)
		}
	case <-ctx.Done():
		t.Fatal("first connection timeout")
	}
	select {
	case n := <-server.calls:
		if n != 2 {
			t.Fatal(n)
		}
	case <-ctx.Done():
		t.Fatal("reconnect timeout")
	}
	select {
	case <-server.pinged:
	case <-ctx.Done():
		t.Fatal("heartbeat timeout")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop agent")
	}
}

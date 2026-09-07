package controllernet

import (
	"bytes"
	"context"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/inventory"
	"github.com/le0xdon/le0xfarm/internal/protocol"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func TestControllerRequiresExplicitDevelopmentMode(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("plaintext allowed without --insecure-dev")
	}
}

func TestPingNonceVerification(t *testing.T) {
	result := &le0xv1.CommandResult{Result: &le0xv1.CommandResult_Pong{Pong: &le0xv1.Pong{Nonce: []byte{1, 2, 3}}}}
	if !pingResultValid(result, []byte{1, 2, 3}) {
		t.Fatal("correct nonce rejected")
	}
	if pingResultValid(result, []byte{1, 2, 4}) {
		t.Fatal("wrong nonce accepted")
	}
	if pingResultValid(&le0xv1.CommandResult{Result: &le0xv1.CommandResult_Status{Status: &le0xv1.Status{}}}, []byte{1, 2, 3}) {
		t.Fatal("non-Pong accepted")
	}
}

func TestInventoryWireTrustBoundary(t *testing.T) {
	host, err := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	valid := &le0xv1.Inventory{HostId: host.String(), Gpus: []*le0xv1.GPU{{DeviceId: "device_0123456789abcdef0123456789abcdef"}}}
	if err := validateInventoryWire(valid, host); err != nil {
		t.Fatal(err)
	}
	if err := validateInventoryWire(&le0xv1.Inventory{HostId: "host_0123456789abcdef0123456789abcde0"}, host); err == nil {
		t.Fatal("mismatched host accepted")
	}
	if err := validateInventoryWire(&le0xv1.Inventory{HostId: host.String(), Gpus: []*le0xv1.GPU{{DeviceId: "bad"}}}, host); err == nil {
		t.Fatal("invalid GPU accepted")
	}
}

func TestControllerHandshakeAndCommands(t *testing.T) {
	var output bytes.Buffer
	server, err := New(Config{InsecureDev: true, Output: testLogger(&output), Inventory: inventory.Local()})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	le0xv1.RegisterAgentControlServer(grpcServer, server)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := le0xv1.NewAgentControlClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	agentID := "agent_0123456789abcdef0123456789abcdef"
	hostID := "host_0123456789abcdef0123456789abcdef"
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: agentID, HostId: hostID, Hostname: "test-host"}}}); err != nil {
		t.Fatal(err)
	}
	message, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got := message.GetHello(); got == nil || got.ControllerId == "" || got.FarmId == "" {
		t.Fatalf("invalid controller hello: %v", message)
	}
	for i, expected := range []string{"Ping", "GetStatus", "GetInventory"} {
		message, err = stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		command := message.GetCommand()
		if command == nil || command.CommandId == "" {
			t.Fatalf("command %d missing: %v", i, message)
		}
		var result *le0xv1.CommandResult
		switch expected {
		case "Ping":
			result = &le0xv1.CommandResult{CommandId: command.CommandId, Result: &le0xv1.CommandResult_Pong{Pong: &le0xv1.Pong{Nonce: command.GetPing().Nonce}}}
		case "GetStatus":
			result = &le0xv1.CommandResult{CommandId: command.CommandId, Result: &le0xv1.CommandResult_Status{Status: &le0xv1.Status{AgentState: "IDLE"}}}
		case "GetInventory":
			result = &le0xv1.CommandResult{CommandId: command.CommandId, Result: &le0xv1.CommandResult_Inventory{Inventory: &le0xv1.Inventory{HostId: hostID}}}
		}
		if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: result}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Heartbeat{Heartbeat: &le0xv1.Heartbeat{ObservedStateRevision: 0}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && (!strings.Contains(output.String(), "PING: OK") || !strings.Contains(output.String(), "STATUS: IDLE")) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(output.String(), "PING: OK") || !strings.Contains(output.String(), "STATUS: IDLE") {
		t.Fatalf("controller output: %s", output.String())
	}
}

func TestControllerConnectionCounterLifecycle(t *testing.T) {
	server, err := New(Config{InsecureDev: true})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	gs := grpc.NewServer()
	le0xv1.RegisterAgentControlServer(gs, server)
	go gs.Serve(listener)
	defer gs.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := grpc.DialContext(ctx, "buf", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := le0xv1.NewAgentControlClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{ProtocolVersion: 1, SchemaVersion: 1, AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "host_0123456789abcdef0123456789abcdef"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for server.ActiveConnections() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.ActiveConnections() != 1 {
		t.Fatalf("counter did not increment: %d", server.ActiveConnections())
	}
	cancel()
	deadline = time.Now().Add(time.Second)
	for server.ActiveConnections() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := server.ActiveConnections(); got != 0 {
		t.Fatalf("counter did not decrement: %d", got)
	}
	if server.ActiveConnections() < 0 {
		t.Fatal("negative counter")
	}
}

func startTestServe(t *testing.T, server *Server, listener *bufconn.Listener, ctx context.Context) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	return done
}

func TestServeShutdownForcesLongLivedStreamToStop(t *testing.T) {
	server, err := New(Config{InsecureDev: true, ShutdownGracePeriod: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTestServe(t, server, listener, ctx)
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	conn, err := grpc.DialContext(clientCtx, "buf", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := le0xv1.NewAgentControlClient(conn).Connect(clientCtx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{ProtocolVersion: 1, SchemaVersion: 1, AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "host_0123456789abcdef0123456789abcdef"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for server.ActiveConnections() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.ActiveConnections() != 1 {
		t.Fatalf("active connection not registered: %d", server.ActiveConnections())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop after grace timeout")
	}
	deadline = time.Now().Add(time.Second)
	for server.ActiveConnections() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := server.ActiveConnections(); got != 0 {
		t.Fatalf("connections after forced shutdown: %d", got)
	}
}

func TestServeShutdownWithoutStreamsIsImmediate(t *testing.T) {
	server, err := New(Config{InsecureDev: true, ShutdownGracePeriod: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTestServe(t, server, listener, ctx)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("idle Serve waited for grace period")
	}
	if server.ActiveConnections() != 0 {
		t.Fatal("idle shutdown changed connection count")
	}
}

func TestControllerRejectsInvalidTrustBoundaryData(t *testing.T) {
	cases := []struct {
		name  string
		hello *le0xv1.AgentHello
		code  farmerr.Code
	}{
		{"agent", &le0xv1.AgentHello{ProtocolVersion: 1, SchemaVersion: 1, AgentId: "bad", HostId: "host_0123456789abcdef0123456789abcdef"}, farmerr.CONFIG_CONFLICT},
		{"host", &le0xv1.AgentHello{ProtocolVersion: 1, SchemaVersion: 1, AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "bad"}, farmerr.CONFIG_CONFLICT},
		{"protocol", &le0xv1.AgentHello{ProtocolVersion: 999, SchemaVersion: 1, AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "host_0123456789abcdef0123456789abcdef"}, farmerr.PROTOCOL_VERSION_MISMATCH},
		{"schema", &le0xv1.AgentHello{ProtocolVersion: 1, SchemaVersion: 999, AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "host_0123456789abcdef0123456789abcdef"}, farmerr.SCHEMA_VERSION_MISMATCH},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, err := New(Config{InsecureDev: true})
			if err != nil {
				t.Fatal(err)
			}
			listener := bufconn.Listen(1024 * 1024)
			gs := grpc.NewServer()
			le0xv1.RegisterAgentControlServer(gs, server)
			go gs.Serve(listener)
			defer gs.Stop()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, err := grpc.DialContext(ctx, "buf", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithInsecure(), grpc.WithBlock())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			stream, err := le0xv1.NewAgentControlClient(conn).Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: tc.hello}}); err != nil {
				t.Fatal(err)
			}
			_, err = stream.Recv()
			if err == nil || !strings.Contains(err.Error(), string(tc.code)) {
				t.Fatalf("error %v, want %s", err, tc.code)
			}
		})
	}
}

func testLogger(buffer *bytes.Buffer) *log.Logger { return log.New(buffer, "", 0) }

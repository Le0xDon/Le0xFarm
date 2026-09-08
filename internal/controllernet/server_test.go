package controllernet

import (
	"context"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllertrust"
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

func TestControllerRequiresTrustStore(t *testing.T) {
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	_, err := New(Config{InsecureDev: true, ControllerID: controllerID, FarmID: farmID})
	code, ok := farmerr.CodeOf(err)
	if !ok || code != farmerr.CONFIG_CONFLICT {
		t.Fatalf("error=%v code=%v", err, code)
	}
}

func TestPairingRequiredAndPairedAgentAcceptedWithoutWindow(t *testing.T) {
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	trust, err := controllertrust.Open(t.TempDir(), controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	connect := func() error {
		server, err := New(Config{InsecureDev: true, ControllerID: controllerID, FarmID: farmID, Trust: trust})
		if err != nil {
			return err
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
			return err
		}
		defer conn.Close()
		stream, err := le0xv1.NewAgentControlClient(conn).Connect(ctx)
		if err != nil {
			return err
		}
		if err = stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: agentID.String(), HostId: hostID.String()}}}); err != nil {
			return err
		}
		_, err = stream.Recv()
		return err
	}
	if err := connect(); err == nil || !strings.Contains(err.Error(), string(farmerr.PAIRING_REQUIRED)) {
		t.Fatalf("unpaired error=%v", err)
	}
	if err := trust.Pair(agentID, hostID); err != nil {
		t.Fatal(err)
	}
	if err := connect(); err != nil {
		t.Fatalf("paired agent rejected: %v", err)
	}
}

func TestControllerPairingTokenLifecycle(t *testing.T) {
	now := time.Unix(100, 0)
	newFixture := func(ttl time.Duration) (*PairingWindow, string, *controllertrust.Store, identity.ControllerID, identity.FarmID) {
		controllerID, _ := identity.NewControllerID()
		farmID, _ := identity.NewFarmID()
		trust, err := controllertrust.Open(t.TempDir(), controllerID, farmID)
		if err != nil {
			t.Fatal(err)
		}
		window, token, _, err := NewPairingWindow(ttl, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return window, token, trust, controllerID, farmID
	}
	attempt := func(window *PairingWindow, trust *controllertrust.Store, controllerID identity.ControllerID, farmID identity.FarmID, agentID identity.AgentID, hostID identity.HostID, token string) error {
		server, err := New(Config{InsecureDev: true, ControllerID: controllerID, FarmID: farmID, Trust: trust, Pairing: window})
		if err != nil {
			return err
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
			return err
		}
		defer conn.Close()
		stream, err := le0xv1.NewAgentControlClient(conn).Connect(ctx)
		if err != nil {
			return err
		}
		if err = stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: agentID.String(), HostId: hostID.String(), EnrollmentToken: token}}}); err != nil {
			return err
		}
		_, err = stream.Recv()
		return err
	}
	t.Run("invalid", func(t *testing.T) {
		w, _, trust, c, f := newFixture(time.Minute)
		a, _ := identity.NewAgentID()
		h, _ := identity.NewHostID()
		err := attempt(w, trust, c, f, a, h, "wrong")
		if err == nil || !strings.Contains(err.Error(), string(farmerr.PAIRING_TOKEN_INVALID)) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		w, token, trust, c, f := newFixture(time.Second)
		now = now.Add(2 * time.Second)
		a, _ := identity.NewAgentID()
		h, _ := identity.NewHostID()
		err := attempt(w, trust, c, f, a, h, token)
		if err == nil || !strings.Contains(err.Error(), string(farmerr.PAIRING_TOKEN_EXPIRED)) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("consumed", func(t *testing.T) {
		now = time.Unix(100, 0)
		w, token, trust, c, f := newFixture(time.Minute)
		a1, _ := identity.NewAgentID()
		h1, _ := identity.NewHostID()
		if err := attempt(w, trust, c, f, a1, h1, token); err != nil {
			t.Fatal(err)
		}
		a2, _ := identity.NewAgentID()
		h2, _ := identity.NewHostID()
		err := attempt(w, trust, c, f, a2, h2, token)
		if err == nil || !strings.Contains(err.Error(), string(farmerr.PAIRING_TOKEN_INVALID)) {
			t.Fatalf("error=%v", err)
		}
	})
}

func newTestServer(t *testing.T, config Config) *Server {
	t.Helper()
	config.InsecureDev = true
	config.ControllerID, _ = identity.NewControllerID()
	config.FarmID, _ = identity.NewFarmID()
	trust, err := controllertrust.Open(t.TempDir(), config.ControllerID, config.FarmID)
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := identity.ParseAgentID("agent_0123456789abcdef0123456789abcdef")
	hostID, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	if err := trust.Pair(agentID, hostID); err != nil {
		t.Fatal(err)
	}
	config.Trust = trust
	server, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return server
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
	var output lockedBuffer
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	trust, err := controllertrust.Open(t.TempDir(), controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	agentIDValue, _ := identity.ParseAgentID("agent_0123456789abcdef0123456789abcdef")
	hostIDValue, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	if err := trust.Pair(agentIDValue, hostIDValue); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{InsecureDev: true, ControllerID: controllerID, FarmID: farmID, Trust: trust, Output: testLogger(&output), Inventory: inventory.Local()})
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
	if got := message.GetHello(); got == nil || got.ControllerId != controllerID.String() || got.FarmId != farmID.String() {
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
	server := newTestServer(t, Config{})
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
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "host_0123456789abcdef0123456789abcdef"}}}); err != nil {
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
	server := newTestServer(t, Config{ShutdownGracePeriod: 20 * time.Millisecond})
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
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "host_0123456789abcdef0123456789abcdef"}}}); err != nil {
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
	server := newTestServer(t, Config{ShutdownGracePeriod: time.Second})
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
		{"agent", &le0xv1.AgentHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: "bad", HostId: "host_0123456789abcdef0123456789abcdef"}, farmerr.CONFIG_CONFLICT},
		{"host", &le0xv1.AgentHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "bad"}, farmerr.CONFIG_CONFLICT},
		{"old-protocol", &le0xv1.AgentHello{ProtocolVersion: 1, SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "host_0123456789abcdef0123456789abcdef"}, farmerr.PROTOCOL_VERSION_MISMATCH},
		{"schema", &le0xv1.AgentHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: 999, AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "host_0123456789abcdef0123456789abcdef"}, farmerr.SCHEMA_VERSION_MISMATCH},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t, Config{})
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
			if tc.name == "old-protocol" && server.ActiveConnections() != 0 {
				t.Fatal("old protocol peer established a runtime session")
			}
		})
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func testLogger(buffer *lockedBuffer) *log.Logger { return log.New(buffer, "", 0) }

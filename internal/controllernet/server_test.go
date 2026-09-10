package controllernet

import (
	"context"
	"errors"
	"io"
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
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/protocol"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	message, err = stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	firstCommand := message.GetCommand()
	if firstCommand == nil || firstCommand.GetGetExecutions() == nil {
		t.Fatalf("first command after hello must be GET_EXECUTIONS: %v", message)
	}
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: &le0xv1.CommandResult{CommandId: firstCommand.CommandId, Result: &le0xv1.CommandResult_Executions{Executions: &le0xv1.Executions{}}}}}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		message, err = stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		command := message.GetCommand()
		if command == nil || command.CommandId == "" {
			t.Fatalf("command %d missing: %v", i, message)
		}
		var result *le0xv1.CommandResult
		switch {
		case command.GetPing() != nil:
			seen["Ping"] = true
			result = &le0xv1.CommandResult{CommandId: command.CommandId, Result: &le0xv1.CommandResult_Pong{Pong: &le0xv1.Pong{Nonce: command.GetPing().Nonce}}}
		case command.GetGetStatus() != nil:
			seen["GetStatus"] = true
			result = &le0xv1.CommandResult{CommandId: command.CommandId, Result: &le0xv1.CommandResult_Status{Status: &le0xv1.Status{AgentState: "IDLE"}}}
		case command.GetGetInventory() != nil:
			seen["GetInventory"] = true
			result = &le0xv1.CommandResult{CommandId: command.CommandId, Result: &le0xv1.CommandResult_Inventory{Inventory: &le0xv1.Inventory{HostId: hostID}}}
		default:
			t.Fatalf("unexpected bootstrap command: %v", command)
		}
		if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: result}}); err != nil {
			t.Fatal(err)
		}
	}
	if !seen["Ping"] || !seen["GetStatus"] || !seen["GetInventory"] {
		t.Fatalf("bootstrap commands missing: %v", seen)
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

type sessionRecorder struct {
	connected    chan SessionInfo
	disconnected chan SessionInfo
	ready        chan SessionInfo
	executions   chan []model.ExecutionObservation
	statuses     chan model.AgentState
	runtime      chan recordedRuntimeResult
}

type recordedRuntimeResult struct {
	request RuntimeRequest
	error   *farmerr.Error
}

func newSessionRecorder() *sessionRecorder {
	return &sessionRecorder{connected: make(chan SessionInfo, 4), disconnected: make(chan SessionInfo, 4), ready: make(chan SessionInfo, 4), executions: make(chan []model.ExecutionObservation, 4), statuses: make(chan model.AgentState, 4), runtime: make(chan recordedRuntimeResult, 4)}
}
func (recorder *sessionRecorder) SessionConnected(info SessionInfo)    { recorder.connected <- info }
func (recorder *sessionRecorder) SessionDisconnected(info SessionInfo) { recorder.disconnected <- info }
func (recorder *sessionRecorder) ExecutionsObserved(_ SessionInfo, values []model.ExecutionObservation, _ uint64) {
	recorder.executions <- values
}
func (recorder *sessionRecorder) InventoryObserved(SessionInfo, model.Inventory) {}
func (recorder *sessionRecorder) StatusObserved(_ SessionInfo, state model.AgentState) {
	recorder.statuses <- state
}
func (recorder *sessionRecorder) SessionReady(info SessionInfo) { recorder.ready <- info }
func (recorder *sessionRecorder) RuntimeResult(_ SessionInfo, request RuntimeRequest, _ *model.ExecutionObservation, runtimeError *farmerr.Error) {
	recorder.runtime <- recordedRuntimeResult{request: request, error: runtimeError}
}

func TestLiveSessionRequiresFreshExecutionsAndChangesEpoch(t *testing.T) {
	server := newTestServer(t, Config{})
	recorder := newSessionRecorder()
	server.SetSessionHandler(recorder)
	listener := bufconn.Listen(1024 * 1024)
	gs := grpc.NewServer()
	le0xv1.RegisterAgentControlServer(gs, server)
	go gs.Serve(listener)
	defer gs.Stop()
	connect := func() (context.CancelFunc, le0xv1.AgentControl_ConnectClient, SessionInfo) {
		ctx, cancel := context.WithCancel(context.Background())
		conn, err := grpc.DialContext(ctx, "buf", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithInsecure(), grpc.WithBlock())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		stream, err := le0xv1.NewAgentControlClient(conn).Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{ProtocolVersion: 3, SchemaVersion: 1, AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "host_0123456789abcdef0123456789abcdef"}}}); err != nil {
			t.Fatal(err)
		}
		if hello, err := stream.Recv(); err != nil || hello.GetHello() == nil {
			t.Fatalf("hello=%v err=%v", hello, err)
		}
		info := <-recorder.connected
		first, err := stream.Recv()
		if err != nil || first.GetCommand().GetGetExecutions() == nil {
			t.Fatalf("first command before fresh observation=%v err=%v", first, err)
		}
		if session, ok := server.Session(info.HostID); !ok || session.Ready {
			t.Fatalf("session ready before GET_EXECUTIONS result: %+v", session)
		}
		request := testRuntimeRequest(t, info.HostID)
		if _, err := server.SendRuntime(context.Background(), info, request); codeOf(err) != farmerr.SERVICE_NOT_READY {
			t.Fatalf("runtime action allowed before fresh observation: %v", err)
		}
		if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: &le0xv1.CommandResult{CommandId: first.GetCommand().CommandId, Result: &le0xv1.CommandResult_Executions{Executions: &le0xv1.Executions{}}}}}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			message, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			command := message.GetCommand()
			result := &le0xv1.CommandResult{CommandId: command.CommandId}
			switch {
			case command.GetGetStatus() != nil:
				result.Result = &le0xv1.CommandResult_Status{Status: &le0xv1.Status{AgentState: "IDLE"}}
			case command.GetGetInventory() != nil:
				result.Result = &le0xv1.CommandResult_Inventory{Inventory: &le0xv1.Inventory{HostId: info.HostID.String()}}
			case command.GetPing() != nil:
				result.Result = &le0xv1.CommandResult_Pong{Pong: &le0xv1.Pong{Nonce: command.GetPing().Nonce}}
			default:
				t.Fatalf("runtime command sent before session READY: %v", command)
			}
			if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: result}}); err != nil {
				t.Fatal(err)
			}
		}
		ready := <-recorder.ready
		if !ready.Ready || ready.ConnectionEpoch != info.ConnectionEpoch {
			t.Fatalf("invalid ready session: %+v", ready)
		}
		return cancel, stream, ready
	}
	cancelFirst, firstStream, first := connect()
	if _, err := server.SendRuntime(context.Background(), first, testRuntimeRequest(t, first.HostID)); err != nil {
		t.Fatalf("fresh READY session rejected runtime action: %v", err)
	}
	if message, err := firstStream.Recv(); err != nil || message.GetCommand().GetStartExecution() == nil {
		t.Fatalf("runtime START not delivered after READY: %v err=%v", message, err)
	} else {
		if err := firstStream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: &le0xv1.CommandResult{CommandId: message.GetCommand().CommandId}}}); err != nil {
			t.Fatal(err)
		}
		select {
		case result := <-recorder.runtime:
			if result.error == nil || result.error.Code != farmerr.INTERNAL_ERROR || result.request.DispatchSequence != 1 {
				t.Fatalf("malformed runtime result was not surfaced as uncertain: %+v", result)
			}
		case <-time.After(time.Second):
			t.Fatal("malformed runtime result disappeared from pending lifecycle")
		}
	}
	cancelFirst()
	disconnected := <-recorder.disconnected
	if disconnected.ConnectionEpoch != first.ConnectionEpoch {
		t.Fatal("disconnect epoch changed")
	}
	cancelSecond, _, second := connect()
	defer cancelSecond()
	if second.ConnectionEpoch <= first.ConnectionEpoch {
		t.Fatalf("connection epoch did not increase: %d -> %d", first.ConnectionEpoch, second.ConnectionEpoch)
	}
}

type blockingConnectServer struct {
	ctx       context.Context
	sendCalls chan struct{}
	release   chan struct{}
}

func (stream *blockingConnectServer) Send(*le0xv1.ControllerMessage) error {
	stream.sendCalls <- struct{}{}
	select {
	case <-stream.release:
		return nil
	case <-stream.ctx.Done():
		return stream.ctx.Err()
	}
}
func (*blockingConnectServer) Recv() (*le0xv1.AgentMessage, error) { return nil, io.EOF }
func (*blockingConnectServer) SetHeader(metadata.MD) error         { return nil }
func (*blockingConnectServer) SendHeader(metadata.MD) error        { return nil }
func (*blockingConnectServer) SetTrailer(metadata.MD)              {}
func (stream *blockingConnectServer) Context() context.Context     { return stream.ctx }
func (*blockingConnectServer) SendMsg(any) error                   { return nil }
func (*blockingConnectServer) RecvMsg(any) error                   { return io.EOF }

func TestRuntimeSendHonorsTimeoutAndRevokesSession(t *testing.T) {
	server := newTestServer(t, Config{RuntimeActionTimeout: 20 * time.Millisecond})
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	agent, _ := identity.ParseAgentID("agent_0123456789abcdef0123456789abcdef")
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	stream := &blockingConnectServer{ctx: streamCtx, sendCalls: make(chan struct{}, 1), release: make(chan struct{})}
	info := SessionInfo{AgentID: agent, HostID: host, ConnectionEpoch: 1, Authenticated: true, Ready: true}
	live := &liveSession{info: info, stream: stream, sendGate: make(chan struct{}, 1), revoked: make(chan struct{}), finished: make(chan struct{}), pending: make(map[string]pendingCommand)}
	server.mu.Lock()
	server.sessions[host] = live
	server.mu.Unlock()

	started := time.Now()
	_, err := server.SendRuntime(context.Background(), info, testRuntimeRequest(t, host))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("bounded send error=%v elapsed=%s", err, time.Since(started))
	}
	if _, ok := server.Session(host); ok {
		t.Fatal("timed-out session retained dispatch authority")
	}
	select {
	case <-stream.sendCalls:
	default:
		t.Fatal("test did not reach blocking stream Send")
	}
	cancelStream()
}

func TestRuntimeSendHonorsCallerCancellation(t *testing.T) {
	server := newTestServer(t, Config{RuntimeActionTimeout: time.Second})
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	agent, _ := identity.ParseAgentID("agent_0123456789abcdef0123456789abcdef")
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	stream := &blockingConnectServer{ctx: streamCtx, sendCalls: make(chan struct{}, 1), release: make(chan struct{})}
	info := SessionInfo{AgentID: agent, HostID: host, ConnectionEpoch: 1, Authenticated: true, Ready: true}
	live := &liveSession{info: info, stream: stream, sendGate: make(chan struct{}, 1), revoked: make(chan struct{}), finished: make(chan struct{}), pending: make(map[string]pendingCommand)}
	server.mu.Lock()
	server.sessions[host] = live
	server.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	request := testRuntimeRequest(t, host)
	go func() {
		_, err := server.SendRuntime(ctx, info, request)
		done <- err
	}()
	<-stream.sendCalls
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation error=%v", err)
	}
	if _, ok := server.Session(host); ok {
		t.Fatal("canceled session retained dispatch authority")
	}
}

func TestMissingRuntimeResultExpiresAsUncertain(t *testing.T) {
	server := newTestServer(t, Config{RuntimeActionTimeout: 20 * time.Millisecond})
	recorder := newSessionRecorder()
	server.SetSessionHandler(recorder)
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	agent, _ := identity.ParseAgentID("agent_0123456789abcdef0123456789abcdef")
	release := make(chan struct{})
	close(release)
	stream := &blockingConnectServer{ctx: context.Background(), sendCalls: make(chan struct{}, 1), release: release}
	info := SessionInfo{AgentID: agent, HostID: host, ConnectionEpoch: 1, Authenticated: true, Ready: true}
	live := &liveSession{info: info, stream: stream, sendGate: make(chan struct{}, 1), revoked: make(chan struct{}), finished: make(chan struct{}), pending: make(map[string]pendingCommand)}
	server.mu.Lock()
	server.sessions[host] = live
	server.mu.Unlock()
	sequence, err := server.SendRuntime(context.Background(), info, testRuntimeRequest(t, host))
	if err != nil || sequence != 1 {
		t.Fatalf("dispatch sequence=%d err=%v", sequence, err)
	}
	select {
	case result := <-recorder.runtime:
		if result.error == nil || result.error.Code != farmerr.INTERNAL_ERROR || result.request.DispatchSequence != sequence {
			t.Fatalf("missing result lifecycle=%+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("missing runtime result did not expire")
	}
	live.pendingMu.Lock()
	pending := len(live.pending)
	live.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("expired runtime request remained pending: %d", pending)
	}
}

func TestReplacedSessionCannotDispatch(t *testing.T) {
	server := newTestServer(t, Config{})
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	agent, _ := identity.ParseAgentID("agent_0123456789abcdef0123456789abcdef")
	oldInfo := SessionInfo{AgentID: agent, HostID: host, ConnectionEpoch: 1, Authenticated: true, Ready: true}
	old := &liveSession{info: oldInfo, stream: &blockingConnectServer{ctx: context.Background(), sendCalls: make(chan struct{}, 1), release: make(chan struct{})}, sendGate: make(chan struct{}, 1), revoked: make(chan struct{}), finished: make(chan struct{}), pending: make(map[string]pendingCommand)}
	newInfo := oldInfo
	newInfo.ConnectionEpoch = 2
	newLive := &liveSession{info: newInfo, stream: old.stream, sendGate: make(chan struct{}, 1), revoked: make(chan struct{}), finished: make(chan struct{}), pending: make(map[string]pendingCommand)}
	server.mu.Lock()
	server.sessions[host] = old
	old.revoke()
	server.sessions[host] = newLive
	server.mu.Unlock()
	if _, err := server.SendRuntime(context.Background(), oldInfo, testRuntimeRequest(t, host)); codeOf(err) != farmerr.SERVICE_NOT_READY {
		t.Fatalf("obsolete session dispatched: %v", err)
	}
}

func TestRevokedLiveSessionCannotEnterTransportSend(t *testing.T) {
	stream := &blockingConnectServer{ctx: context.Background(), sendCalls: make(chan struct{}, 1), release: make(chan struct{})}
	live := &liveSession{stream: stream, sendGate: make(chan struct{}, 1), revoked: make(chan struct{}), pending: make(map[string]pendingCommand)}
	live.revoke()
	command := &le0xv1.CommandEnvelope{CommandId: "revoked", Command: &le0xv1.CommandEnvelope_GetExecutions{GetExecutions: &le0xv1.GetExecutions{}}}
	if _, err := live.send(context.Background(), command, pendingCommand{kind: "GET_EXECUTIONS"}); codeOf(err) != farmerr.SERVICE_NOT_READY {
		t.Fatalf("revoked session error=%v", err)
	}
	select {
	case <-stream.sendCalls:
		t.Fatal("revoked session entered transport Send")
	default:
	}
}

func TestReadySessionRefreshesExecutionsAndStatusOnHeartbeat(t *testing.T) {
	server := newTestServer(t, Config{})
	recorder := newSessionRecorder()
	server.SetSessionHandler(recorder)
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
	host := "host_0123456789abcdef0123456789abcdef"
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{ProtocolVersion: 3, SchemaVersion: 1, AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: host}}}); err != nil {
		t.Fatal(err)
	}
	if message, err := stream.Recv(); err != nil || message.GetHello() == nil {
		t.Fatalf("hello=%v err=%v", message, err)
	}
	<-recorder.connected
	bootstrap, err := stream.Recv()
	if err != nil || bootstrap.GetCommand().GetGetExecutions() == nil {
		t.Fatalf("bootstrap=%v err=%v", bootstrap, err)
	}
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: &le0xv1.CommandResult{CommandId: bootstrap.GetCommand().CommandId, Result: &le0xv1.CommandResult_Executions{Executions: &le0xv1.Executions{}}}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		message, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		command := message.GetCommand()
		result := &le0xv1.CommandResult{CommandId: command.CommandId}
		switch {
		case command.GetGetStatus() != nil:
			result.Result = &le0xv1.CommandResult_Status{Status: &le0xv1.Status{AgentState: "IDLE"}}
		case command.GetGetInventory() != nil:
			result.Result = &le0xv1.CommandResult_Inventory{Inventory: &le0xv1.Inventory{HostId: host}}
		case command.GetPing() != nil:
			result.Result = &le0xv1.CommandResult_Pong{Pong: &le0xv1.Pong{Nonce: command.GetPing().Nonce}}
		default:
			t.Fatalf("unexpected bootstrap command: %v", command)
		}
		if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: result}}); err != nil {
			t.Fatal(err)
		}
	}
	<-recorder.ready
	<-recorder.executions
	<-recorder.statuses
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Heartbeat{Heartbeat: &le0xv1.Heartbeat{Timestamp: timestamppb.Now()}}}); err != nil {
		t.Fatal(err)
	}
	seenExecutions, seenStatus := false, false
	for i := 0; i < 2; i++ {
		message, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		command := message.GetCommand()
		result := &le0xv1.CommandResult{CommandId: command.CommandId}
		switch {
		case command.GetGetExecutions() != nil:
			seenExecutions = true
			result.Result = &le0xv1.CommandResult_Executions{Executions: &le0xv1.Executions{}}
		case command.GetGetStatus() != nil:
			seenStatus = true
			result.Result = &le0xv1.CommandResult_Status{Status: &le0xv1.Status{AgentState: "ERROR"}}
		default:
			t.Fatalf("unexpected refresh command: %v", command)
		}
		if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: result}}); err != nil {
			t.Fatal(err)
		}
	}
	if !seenExecutions || !seenStatus {
		t.Fatalf("refresh commands executions=%t status=%t", seenExecutions, seenStatus)
	}
	select {
	case <-recorder.executions:
	case <-time.After(time.Second):
		t.Fatal("refreshed execution observation was not delivered")
	}
	select {
	case state := <-recorder.statuses:
		if state != model.AgentStateError {
			t.Fatalf("refreshed status=%s", state)
		}
	case <-time.After(time.Second):
		t.Fatal("refreshed Agent status was not delivered")
	}
}

func testRuntimeRequest(t *testing.T, hostID identity.HostID) RuntimeRequest {
	t.Helper()
	workloadID, _ := identity.ParseWorkloadID("workload_0123456789abcdef0123456789abcdef")
	executionID, _ := identity.ParseExecutionID("execution_0123456789abcdef0123456789abcdef")
	ownership := model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: 1, ResolvedHash: "sha256:" + strings.Repeat("a", 64), HostID: hostID, CPU: true}
	return RuntimeRequest{Kind: RuntimeStart, WorkloadID: workloadID, DesiredGeneration: 1, ExecutionID: executionID, ResolvedHash: ownership.ResolvedHash, Plan: model.ExecutionPlan{ExecutionID: executionID, Ownership: ownership, HostID: hostID, Executable: "/bin/sleep", Args: []string{"1"}, RestartPolicy: model.RestartNever}}
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
		{"v2-agent-v3-controller", &le0xv1.AgentHello{ProtocolVersion: 2, SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: "agent_0123456789abcdef0123456789abcdef", HostId: "host_0123456789abcdef0123456789abcdef"}, farmerr.PROTOCOL_VERSION_MISMATCH},
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

package agentnet

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/agentpki"
	"github.com/le0xdon/le0xfarm/internal/agenttrust"
	"github.com/le0xdon/le0xfarm/internal/controllernet"
	"github.com/le0xdon/le0xfarm/internal/controllerpki"
	"github.com/le0xdon/le0xfarm/internal/controllertrust"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/inventory"
	"github.com/le0xdon/le0xfarm/internal/minerruntime"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/protocol"
	"github.com/le0xdon/le0xfarm/internal/runtime/supervisor"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func TestParseGenericMinerPlanValidatesTypedReferences(t *testing.T) {
	id := "execution_0123456789abcdef0123456789abcdef"
	hostID, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	base := &le0xv1.ExecutionPlan{ExecutionId: id, Ownership: testWireOwnership(hostID), Miner: &le0xv1.MinerSpec{AdapterId: "xmrig", SpecVersion: 1, PackageId: "package_0123456789abcdef0123456789abcdef", PackageVersion: "6.26.0", Mode: "MINING", Endpoint: &le0xv1.MiningEndpoint{Address: "pool.example:443", Tls: true, User: "public-login", Password: "secret", Worker: "worker-1"}}}
	plan, err := parsePlan(base)
	if err != nil || plan.Miner == nil || plan.Miner.WalletID != nil || plan.Miner.PoolID != nil || plan.Miner.Endpoint == nil || !plan.Miner.Endpoint.TLS || plan.Miner.Endpoint.Password != "secret" || plan.Miner.Endpoint.Worker != "worker-1" || plan.Ownership.DesiredGeneration != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	badHash := proto.Clone(base).(*le0xv1.ExecutionPlan)
	badHash.Ownership.ResolvedHash = "sha256:ABC"
	if _, err := parsePlan(badHash); err == nil {
		t.Fatal("invalid ResolvedHash accepted")
	}
	wrongHost, _ := identity.NewHostID()
	if _, err := parsePlanForHost(base, wrongHost); err == nil {
		t.Fatal("wrong target HostID accepted")
	}
}

func testWireOwnership(hostID identity.HostID) *le0xv1.WorkloadOwnership {
	return &le0xv1.WorkloadOwnership{WorkloadId: "workload_0123456789abcdef0123456789abcdef", DesiredGeneration: 1, ResolvedHash: "sha256:" + strings.Repeat("a", 64), HostId: hostID.String(), ResourceClaim: &le0xv1.ResourceClaim{Cpu: true}}
}

func TestAgentRejectsMalformedOwnershipMetadata(t *testing.T) {
	hostID, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	base := &le0xv1.ExecutionPlan{ExecutionId: "execution_0123456789abcdef0123456789abcdef", Executable: "/bin/sleep", Args: []string{"1"}, RestartPolicy: "NEVER", Ownership: testWireOwnership(hostID)}
	cases := map[string]func(*le0xv1.ExecutionPlan){
		"missing":       func(plan *le0xv1.ExecutionPlan) { plan.Ownership = nil },
		"workload":      func(plan *le0xv1.ExecutionPlan) { plan.Ownership.WorkloadId = "bad" },
		"generation":    func(plan *le0xv1.ExecutionPlan) { plan.Ownership.DesiredGeneration = 0 },
		"hash":          func(plan *le0xv1.ExecutionPlan) { plan.Ownership.ResolvedHash = "sha256:" + strings.Repeat("A", 64) },
		"host":          func(plan *le0xv1.ExecutionPlan) { plan.Ownership.HostId = "bad" },
		"claim":         func(plan *le0xv1.ExecutionPlan) { plan.Ownership.ResourceClaim = &le0xv1.ResourceClaim{} },
		"claim-missing": func(plan *le0xv1.ExecutionPlan) { plan.Ownership.ResourceClaim = nil },
		"duplicate-gpus": func(plan *le0xv1.ExecutionPlan) {
			plan.Ownership.ResourceClaim = &le0xv1.ResourceClaim{DeviceIds: []string{"device_0123456789abcdef0123456789abcdef", "device_0123456789abcdef0123456789abcdef"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			plan := proto.Clone(base).(*le0xv1.ExecutionPlan)
			mutate(plan)
			if _, err := parsePlanForHost(plan, hostID); err == nil {
				t.Fatal("malformed ownership accepted")
			}
		})
	}
	wrongHost, _ := identity.NewHostID()
	if _, err := parsePlanForHost(base, wrongHost); err == nil {
		t.Fatal("mismatched target HostID accepted")
	}
}

func TestAgentRetainsAndEchoesOwnershipMetadata(t *testing.T) {
	hostID, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	wirePlan := &le0xv1.ExecutionPlan{ExecutionId: "execution_0123456789abcdef0123456789abcdef", Executable: "/bin/sleep", Args: []string{"10"}, RestartPolicy: "NEVER", Ownership: testWireOwnership(hostID)}
	plan, err := parsePlanForHost(wirePlan, hostID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := supervisor.New(supervisor.Config{StopGrace: 50 * time.Millisecond})
	defer runtime.Shutdown(context.Background())
	snapshot, _, err := runtime.Start(plan)
	if err != nil {
		t.Fatal(err)
	}
	echoed := wireExecution(snapshot)
	if echoed.GetOwnership().GetWorkloadId() != wirePlan.Ownership.WorkloadId || echoed.GetOwnership().GetDesiredGeneration() != 1 || echoed.GetOwnership().GetResolvedHash() != wirePlan.Ownership.ResolvedHash || echoed.GetOwnership().GetHostId() != hostID.String() || !echoed.GetOwnership().GetResourceClaim().GetCpu() {
		t.Fatalf("ownership metadata was not retained: %+v", echoed.GetOwnership())
	}
}

func TestControllerFacingTelemetryIsAdapterNeutral(t *testing.T) {
	executionID, _ := identity.NewExecutionID()
	hashrate := 77.25
	healthy := true
	accepted := uint64(0)
	wire := wireObservation(minerruntime.Observation{Process: supervisor.Snapshot{ExecutionID: executionID, State: model.ExecutionRunning, PID: 42}, Telemetry: &model.MinerTelemetry{AdapterID: "test-no-http", MinerVersion: "1.0", HashrateShortHPS: &hashrate, Health: model.MinerHealthMining, UsefulWork: &model.UsefulWorkEvidence{Provider: "test-no-http", Availability: model.TelemetryAvailable, RuntimeHealthy: &healthy, UsefulWork: model.UsefulWorkConfirmed, AcceptedWork: &accepted, Upstream: model.UpstreamUnknown, Metrics: []model.WorkMetric{{Kind: "CUSTOM_THROUGHPUT", Unit: "UNIT/S", Value: 0}}, Confidence: model.EvidenceConfidenceAdapterReported}}})
	encoded, err := proto.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var decoded le0xv1.Execution
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GetMinerTelemetry().GetAdapterId() != "test-no-http" || decoded.GetMinerTelemetry().GetHashrateShortHps() != hashrate || decoded.GetUsefulWork().GetUsefulWork() != "CONFIRMED" || decoded.GetUsefulWork().AcceptedWork == nil || decoded.GetUsefulWork().GetAcceptedWork() != 0 || decoded.GetUsefulWork().GetMetrics()[0].GetKind() != "CUSTOM_THROUGHPUT" {
		t.Fatalf("generic adapter telemetry did not reach Controller-facing wire model: %+v", decoded.GetMinerTelemetry())
	}
}

type helloServer struct {
	le0xv1.UnimplementedAgentControlServer
	controllerID identity.ControllerID
	farmID       identity.FarmID
}

type oldProtocolServer struct {
	le0xv1.UnimplementedAgentControlServer
	controllerID identity.ControllerID
	farmID       identity.FarmID
	result       chan bool
}

func (s oldProtocolServer) Connect(stream le0xv1.AgentControl_ConnectServer) error {
	executed := false
	defer func() { s.result <- executed }()
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Hello{Hello: &le0xv1.ControllerHello{ProtocolVersion: 2, SchemaVersion: uint32(protocol.CurrentSchemaVersion), ControllerId: s.controllerID.String(), FarmId: s.farmID.String()}}}); err != nil {
		return err
	}
	if err := stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Command{Command: &le0xv1.CommandEnvelope{CommandId: "must-not-run", Command: &le0xv1.CommandEnvelope_GetStatus{GetStatus: &le0xv1.GetStatus{}}}}}); err != nil {
		return err
	}
	message, err := stream.Recv()
	executed = err == nil && message.GetCommandResult() != nil
	return err
}

type switchingListener struct {
	mu       sync.RWMutex
	listener *bufconn.Listener
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (s *switchingListener) Dial() (net.Conn, error) {
	s.mu.RLock()
	listener := s.listener
	s.mu.RUnlock()
	return listener.Dial()
}

func (s *switchingListener) Set(listener *bufconn.Listener) {
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
}

func (s helloServer) Connect(stream le0xv1.AgentControl_ConnectServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Hello{Hello: &le0xv1.ControllerHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), ControllerId: s.controllerID.String(), FarmId: s.farmID.String()}}})
}

func testLogger() *log.Logger { return log.New(&bytes.Buffer{}, "", 0) }

func TestAgentNetworkRequiresTrustDirectory(t *testing.T) {
	err := Run(context.Background(), Config{InsecureDev: true})
	code, ok := farmerr.CodeOf(err)
	if !ok || code != farmerr.CONFIG_CONFLICT {
		t.Fatalf("error=%v code=%v", err, code)
	}
}

func TestAgentConnectsAndSendsHeartbeat(t *testing.T) {
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	trust, err := controllertrust.Open(t.TempDir(), controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.Pair(agentID, hostID); err != nil {
		t.Fatal(err)
	}
	server, err := controllernet.New(controllernet.Config{InsecureDev: true, ControllerID: controllerID, FarmID: farmID, Trust: trust})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	le0xv1.RegisterAgentControlServer(grpcServer, server)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	trustDir := t.TempDir()
	if err := agenttrust.Save(trustDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err = connectOnce(ctx, Config{Target: "buf", InsecureDev: true, TrustDir: trustDir, AgentID: agentID, HostID: hostID, Hostname: "test", Inventory: inventory.Local(), HeartbeatInterval: 20 * time.Millisecond, Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: testLogger()})
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

func TestNewAgentRejectsOldControllerBeforeRuntimeCommand(t *testing.T) {
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	dir := t.TempDir()
	if err := agenttrust.Save(dir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}); err != nil {
		t.Fatal(err)
	}
	results := make(chan bool, 1)
	listener := bufconn.Listen(1024 * 1024)
	gs := grpc.NewServer()
	le0xv1.RegisterAgentControlServer(gs, oldProtocolServer{controllerID: controllerID, farmID: farmID, result: results})
	go gs.Serve(listener)
	defer gs.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := connectOnce(ctx, Config{Target: "buf", InsecureDev: true, TrustDir: dir, AgentID: agentID, HostID: hostID, Inventory: inventory.Local(), Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: testLogger()})
	if code, ok := farmerr.CodeOf(err); !ok || code != farmerr.PROTOCOL_VERSION_MISMATCH || !nonTransient(err) {
		t.Fatalf("error=%v code=%v", err, code)
	}
	select {
	case executed := <-results:
		if executed {
			t.Fatal("runtime command was accepted after protocol mismatch")
		}
	case <-time.After(time.Second):
		t.Fatal("old Controller stream did not close")
	}
}

func TestControllerIdentityMismatchIsNonTransientAndDoesNotOverwriteTrust(t *testing.T) {
	trustedController, _ := identity.NewControllerID()
	trustedFarm, _ := identity.NewFarmID()
	otherController, _ := identity.NewControllerID()
	dir := t.TempDir()
	if err := agenttrust.Save(dir, agenttrust.Binding{ControllerID: trustedController, FarmID: trustedFarm}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dir + "/controller.json")
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	gs := grpc.NewServer()
	le0xv1.RegisterAgentControlServer(gs, helloServer{controllerID: otherController, farmID: trustedFarm})
	go gs.Serve(listener)
	defer gs.Stop()
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = connectOnce(ctx, Config{Target: "buf", InsecureDev: true, TrustDir: dir, AgentID: agentID, HostID: hostID, Inventory: inventory.Local(), Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: testLogger()})
	code, ok := farmerr.CodeOf(err)
	if !ok || code != farmerr.CONTROLLER_IDENTITY_MISMATCH || !nonTransient(err) {
		t.Fatalf("error=%v code=%v", err, code)
	}
	after, _ := os.ReadFile(dir + "/controller.json")
	if !bytes.Equal(before, after) {
		t.Fatal("controller.json changed")
	}
}

func TestCommandMappingPreservesNonce(t *testing.T) {
	nonce := []byte{0, 1, 2, 255}
	ping := &le0xv1.CommandEnvelope{CommandId: "x", Command: &le0xv1.CommandEnvelope_Ping{Ping: &le0xv1.Ping{Nonce: nonce}}}
	if !bytes.Equal(ping.GetPing().GetNonce(), nonce) {
		t.Fatal("nonce changed")
	}
	if uint32(protocol.CurrentProtocolVersion) != 6 {
		t.Fatal("unexpected protocol test baseline")
	}
}

type reconnectServer struct {
	le0xv1.UnimplementedAgentControlServer
	calls        chan int
	count        int
	pinged       chan struct{}
	controllerID identity.ControllerID
	farmID       identity.FarmID
}

func (s *reconnectServer) Connect(stream le0xv1.AgentControl_ConnectServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	s.count++
	n := s.count
	s.calls <- n
	if err := stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Hello{Hello: &le0xv1.ControllerHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), ControllerId: s.controllerID.String(), FarmId: s.farmID.String()}}}); err != nil {
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
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	server := &reconnectServer{calls: make(chan int, 4), pinged: make(chan struct{}), controllerID: controllerID, farmID: farmID}
	listener := bufconn.Listen(1024 * 1024)
	gs := grpc.NewServer()
	le0xv1.RegisterAgentControlServer(gs, server)
	go gs.Serve(listener)
	defer gs.Stop()
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	trustDir := t.TempDir()
	if err := agenttrust.Save(trustDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Target: "buf", InsecureDev: true, TrustDir: trustDir, AgentID: agentID, HostID: hostID, Hostname: "test", Inventory: inventory.Local(), HeartbeatInterval: 10 * time.Millisecond, ReconnectInitial: time.Millisecond, ReconnectMax: 4 * time.Millisecond, Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: testLogger()})
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

func TestSecureProductionAgentRejectsRawExecutionAfterMutualTLS(t *testing.T) {
	controllerDir := t.TempDir()
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	pki, err := controllerpki.Initialize(controllerDir, controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := controllertrust.Open(controllerDir, controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	window, token, _, err := controllernet.NewPairingWindow(time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	executionID, _ := identity.NewExecutionID()
	runtimeCommand := &le0xv1.CommandEnvelope{Command: &le0xv1.CommandEnvelope_StartExecution{StartExecution: &le0xv1.StartExecution{Plan: &le0xv1.ExecutionPlan{ExecutionId: executionID.String(), Executable: "/bin/sleep", Args: []string{"60"}, RestartPolicy: string(model.RestartNever), Ownership: testWireOwnership(hostID)}}}}
	var controllerOutput lockedBuffer
	server, err := controllernet.New(controllernet.Config{ControllerID: controllerID, FarmID: farmID, Trust: trust, Pairing: window, PKI: pki, RuntimeCommands: []*le0xv1.CommandEnvelope{runtimeCommand}, RuntimeTarget: agentID, Output: log.New(&controllerOutput, "", 0), ShutdownGracePeriod: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	dialer := &switchingListener{listener: listener}
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	doneServer := make(chan error, 1)
	go func() { doneServer <- server.Serve(serverCtx, listener) }()
	agentDir := t.TempDir()
	if err := agenttrust.Save(agentDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}); err != nil {
		t.Fatal(err)
	}
	var output lockedBuffer
	runtimeSupervisor := supervisor.New(supervisor.Config{StopGrace: 100 * time.Millisecond})
	defer runtimeSupervisor.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Target: "buf", TrustDir: agentDir, EnrollmentToken: token, TLSFingerprint: pki.ServerFingerprint(), AgentID: agentID, HostID: hostID, Hostname: "secure-test", Inventory: inventory.Local(), Supervisor: runtimeSupervisor, HeartbeatInterval: 10 * time.Millisecond, ReconnectInitial: time.Millisecond, ReconnectMax: 5 * time.Millisecond, Dialer: func(context.Context, string) (net.Conn, error) { return dialer.Dial() }, Output: log.New(&output, "", 0)})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(output.String(), "Connected to Controller") {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(output.String(), "TLS enrollment complete") || !strings.Contains(output.String(), "Connected to Controller") {
		t.Fatalf("output=%s", output.String())
	}
	if _, err := agentpki.Load(agentDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}, agentID, hostID); err != nil {
		t.Fatal(err)
	}
	if record, ok := trust.Find(agentID); !ok || record.HostID != hostID {
		t.Fatal("Controller trust not persisted")
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(controllerOutput.String(), string(farmerr.PERMISSION_DENIED)) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(controllerOutput.String(), string(farmerr.PERMISSION_DENIED)) {
		t.Fatalf("production raw START was not rejected: %s", controllerOutput.String())
	}
	if snapshot, ok := runtimeSupervisor.Get(executionID); ok {
		t.Fatalf("production Agent executed raw plan: %+v", snapshot)
	}
	cancel()
	<-done
	stopServer()
	<-doneServer
}

func TestSecureBootstrapRejectsWrongFingerprint(t *testing.T) {
	controllerDir := t.TempDir()
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	pki, err := controllerpki.Initialize(controllerDir, controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := controllertrust.Open(controllerDir, controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	window, token, _, _ := controllernet.NewPairingWindow(time.Minute, nil)
	server, err := controllernet.New(controllernet.Config{ControllerID: controllerID, FarmID: farmID, Trust: trust, Pairing: window, PKI: pki})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	serverCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go server.Serve(serverCtx, listener)
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = bootstrap(ctx, Config{Target: "buf", TrustDir: t.TempDir(), EnrollmentToken: token, TLSFingerprint: "SHA256:" + strings.Repeat("00", 32), AgentID: agentID, HostID: hostID, Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: testLogger()}, agenttrust.Binding{}, false)
	code, ok := farmerr.CodeOf(err)
	if !ok || code != farmerr.TLS_FINGERPRINT_MISMATCH || !nonTransient(err) {
		t.Fatalf("error=%v code=%v", err, code)
	}
}

func TestSecureAgentRejectsForeignControllerCANonTransient(t *testing.T) {
	root := t.TempDir()
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	trustedDir := filepath.Join(root, "trusted")
	foreignDir := filepath.Join(root, "foreign")
	agentDir := filepath.Join(root, "agent")
	for _, d := range []string{trustedDir, foreignDir, agentDir} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	trustedPKI, err := controllerpki.Initialize(trustedDir, controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	foreignPKI, err := controllerpki.Initialize(foreignDir, controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	enrollment, err := agentpki.NewEnrollment(agentID, hostID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := trustedPKI.IssueAgent(enrollment.CSRDER, agentID, hostID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	binding := agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}
	if err := agenttrust.Save(agentDir, binding); err != nil {
		t.Fatal(err)
	}
	if err := agentpki.Save(agentDir, enrollment, cert, trustedPKI.CACertificateDER(), binding, agentID, hostID); err != nil {
		t.Fatal(err)
	}
	trust, err := controllertrust.Open(foreignDir, controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.Pair(agentID, hostID); err != nil {
		t.Fatal(err)
	}
	server, err := controllernet.New(controllernet.Config{ControllerID: controllerID, FarmID: farmID, Trust: trust, PKI: foreignPKI})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	serverCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go server.Serve(serverCtx, listener)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = Run(ctx, Config{Target: "buf", TrustDir: agentDir, AgentID: agentID, HostID: hostID, Inventory: inventory.Local(), Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: testLogger()})
	code, ok := farmerr.CodeOf(err)
	if !ok || code != farmerr.TLS_IDENTITY_MISMATCH || !nonTransient(err) {
		t.Fatalf("error=%v code=%v", err, code)
	}
}

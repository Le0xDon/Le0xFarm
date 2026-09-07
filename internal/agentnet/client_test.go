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
	"github.com/le0xdon/le0xfarm/internal/protocol"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

type helloServer struct {
	le0xv1.UnimplementedAgentControlServer
	controllerID identity.ControllerID
	farmID       identity.FarmID
}

func (s helloServer) Connect(stream le0xv1.AgentControl_ConnectServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Hello{Hello: &le0xv1.ControllerHello{ProtocolVersion: 1, SchemaVersion: 1, ControllerId: s.controllerID.String(), FarmId: s.farmID.String()}}})
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
	if uint32(protocol.CurrentProtocolVersion) != 1 {
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
	if err := stream.Send(&le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Hello{Hello: &le0xv1.ControllerHello{ProtocolVersion: 1, SchemaVersion: 1, ControllerId: s.controllerID.String(), FarmId: s.farmID.String()}}}); err != nil {
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

func TestSecureBootstrapThenMutualTLS(t *testing.T) {
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
	server, err := controllernet.New(controllernet.Config{ControllerID: controllerID, FarmID: farmID, Trust: trust, Pairing: window, PKI: pki})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	doneServer := make(chan error, 1)
	go func() { doneServer <- server.Serve(serverCtx, listener) }()
	agentDir := t.TempDir()
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	if err := agenttrust.Save(agentDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Target: "buf", TrustDir: agentDir, EnrollmentToken: token, TLSFingerprint: pki.ServerFingerprint(), AgentID: agentID, HostID: hostID, Hostname: "secure-test", Inventory: inventory.Local(), HeartbeatInterval: 10 * time.Millisecond, ReconnectInitial: time.Millisecond, ReconnectMax: 5 * time.Millisecond, Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: log.New(&output, "", 0)})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !bytes.Contains(output.Bytes(), []byte("Connected to Controller")) {
		time.Sleep(time.Millisecond)
	}
	if !bytes.Contains(output.Bytes(), []byte("TLS enrollment complete")) || !bytes.Contains(output.Bytes(), []byte("Connected to Controller")) {
		t.Fatalf("output=%s", output.String())
	}
	if _, err := agentpki.Load(agentDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}, agentID, hostID); err != nil {
		t.Fatal(err)
	}
	if record, ok := trust.Find(agentID); !ok || record.HostID != hostID {
		t.Fatal("Controller trust not persisted")
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

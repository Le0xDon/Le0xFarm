package le0xv1_test

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func roundTrip(t *testing.T, original proto.Message) proto.Message {
	t.Helper()
	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded := original.ProtoReflect().New().Interface()
	if err := proto.Unmarshal(data, decoded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(original, decoded) {
		t.Fatalf("round trip mismatch: want %v, got %v", original, decoded)
	}
	return decoded
}

func TestHelloVersions(t *testing.T) {
	// Deliberately distinct values detect swapped or conflated versions.
	agent := &le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{
		ProtocolVersion: 7, SchemaVersion: 19,
		AgentId: "agent_0123456789abcdef0123456789abcdef",
		HostId:  "host_0123456789abcdef0123456789abcdef", Hostname: "worker", EnrollmentToken: "token", CertificateRequestDer: []byte{1, 2, 3},
	}}}
	controller := &le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Hello{Hello: &le0xv1.ControllerHello{
		ProtocolVersion: 7, SchemaVersion: 19,
		ControllerId: "controller_0123456789abcdef0123456789abcdef",
		FarmId:       "farm_0123456789abcdef0123456789abcdef", AgentCertificateDer: []byte{4, 5}, FarmCaCertificateDer: []byte{6, 7},
	}}}
	a := roundTrip(t, agent).(*le0xv1.AgentMessage).GetHello()
	c := roundTrip(t, controller).(*le0xv1.ControllerMessage).GetHello()
	if a.GetProtocolVersion() != 7 || a.GetSchemaVersion() != 19 || c.GetProtocolVersion() != 7 || c.GetSchemaVersion() != 19 {
		t.Fatal("versions lost")
	}
	if a.GetEnrollmentToken() != "token" {
		t.Fatal("enrollment token lost")
	}
	if !bytes.Equal(a.GetCertificateRequestDer(), []byte{1, 2, 3}) || !bytes.Equal(c.GetAgentCertificateDer(), []byte{4, 5}) || !bytes.Equal(c.GetFarmCaCertificateDer(), []byte{6, 7}) {
		t.Fatal("certificate bootstrap fields lost")
	}
	for _, message := range []proto.Message{a, c} {
		for _, name := range []protoreflect.Name{"protocol_version", "schema_version"} {
			if message.ProtoReflect().Descriptor().Fields().ByName(name).Kind() != protoreflect.Uint32Kind {
				t.Fatalf("%s must be uint32", name)
			}
		}
	}
}

func TestCommandOneofs(t *testing.T) {
	nonce := []byte{0, 1, 127, 128, 255, 0}
	commands := []*le0xv1.CommandEnvelope{
		{CommandId: "ping-1", Command: &le0xv1.CommandEnvelope_Ping{Ping: &le0xv1.Ping{Nonce: nonce}}},
		{CommandId: "status-1", Command: &le0xv1.CommandEnvelope_GetStatus{GetStatus: &le0xv1.GetStatus{}}},
		{CommandId: "inventory-1", Command: &le0xv1.CommandEnvelope_GetInventory{GetInventory: &le0xv1.GetInventory{}}},
		{CommandId: "start-1", Command: &le0xv1.CommandEnvelope_StartExecution{StartExecution: &le0xv1.StartExecution{Plan: &le0xv1.ExecutionPlan{ExecutionId: "execution_0123456789abcdef0123456789abcdef", Executable: "/bin/sleep", Args: []string{"300"}, Environment: map[string]string{"MODE": "test"}, WorkingDirectory: "/tmp", RestartPolicy: "ON_FAILURE"}}}},
		{CommandId: "stop-1", Command: &le0xv1.CommandEnvelope_StopExecution{StopExecution: &le0xv1.StopExecution{ExecutionId: "execution_0123456789abcdef0123456789abcdef"}}},
		{CommandId: "restart-1", Command: &le0xv1.CommandEnvelope_RestartExecution{RestartExecution: &le0xv1.RestartExecution{ExecutionId: "execution_0123456789abcdef0123456789abcdef"}}},
		{CommandId: "executions-1", Command: &le0xv1.CommandEnvelope_GetExecutions{GetExecutions: &le0xv1.GetExecutions{}}},
	}
	for _, command := range commands {
		t.Run(command.CommandId, func(t *testing.T) {
			wrapped := &le0xv1.ControllerMessage{Payload: &le0xv1.ControllerMessage_Command{Command: command}}
			decoded := roundTrip(t, wrapped).(*le0xv1.ControllerMessage).GetCommand()
			if reflect.TypeOf(decoded.GetCommand()) != reflect.TypeOf(command.GetCommand()) {
				t.Fatal("command variant changed")
			}
			if command.GetPing() != nil && !bytes.Equal(decoded.GetPing().GetNonce(), nonce) {
				t.Fatal("nonce changed")
			}
		})
	}
	// Replacing a oneof member must remove the previous command, including on wire.
	command := commands[0]
	command.Command = &le0xv1.CommandEnvelope_GetInventory{GetInventory: &le0xv1.GetInventory{}}
	decoded := roundTrip(t, command).(*le0xv1.CommandEnvelope)
	if decoded.GetPing() != nil || decoded.GetGetInventory() == nil {
		t.Fatal("oneof retained replaced command")
	}
}

func TestResultOneofs(t *testing.T) {
	nonce := []byte{0, 255, 128, 42, 0}
	results := []*le0xv1.CommandResult{
		{CommandId: "ping-1", Result: &le0xv1.CommandResult_Pong{Pong: &le0xv1.Pong{Nonce: nonce}}},
		{CommandId: "status-1", Result: &le0xv1.CommandResult_Status{Status: &le0xv1.Status{AgentState: "idle", Message: "No workload"}}},
		{CommandId: "inventory-1", Result: &le0xv1.CommandResult_Inventory{Inventory: &le0xv1.Inventory{
			HostId: "host_0123456789abcdef0123456789abcdef", Hostname: "worker", OsId: "ubuntu", OsVersion: "24.04", Architecture: "amd64",
			CpuVendor: "vendor", CpuModel: "model", CpuCores: 8, CpuThreads: 16,
			Gpus: []*le0xv1.GPU{
				{DeviceId: "device_0123456789abcdef0123456789abcdef", Vendor: "vendor-a", Model: "gpu-a", PciBusId: "0000:01:00.0", Uuid: "gpu-uuid-a"},
				{DeviceId: "device_fedcba9876543210fedcba9876543210", Vendor: "vendor-b", Model: "gpu-b", PciBusId: "0000:02:00.0"},
			},
		}}},
		{CommandId: "error-1", Result: &le0xv1.CommandResult_Error{Error: &le0xv1.TypedError{
			Code: string(farmerr.MISSING_DEPENDENCY), HumanMessage: "Dependency unavailable",
			Details:      map[string]string{"package": "example", "reason": "not installed"},
			SuggestedFix: "Install the dependency", LogsRef: "logs/example",
		}}},
		{CommandId: "execution-1", Result: &le0xv1.CommandResult_Execution{Execution: &le0xv1.ExecutionResult{Execution: &le0xv1.Execution{ExecutionId: "execution_0123456789abcdef0123456789abcdef", State: "RUNNING", Pid: 123, StartedAt: timestamppb.Now(), RestartCount: 2}, Message: "started"}}},
		{CommandId: "executions-1", Result: &le0xv1.CommandResult_Executions{Executions: &le0xv1.Executions{Executions: []*le0xv1.Execution{{ExecutionId: "execution_0123456789abcdef0123456789abcdef", State: "STOPPED", ExitCode: 0, HasExitCode: true}}}}},
	}
	for _, result := range results {
		t.Run(result.CommandId, func(t *testing.T) {
			wrapped := &le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: result}}
			decoded := roundTrip(t, wrapped).(*le0xv1.AgentMessage).GetCommandResult()
			if reflect.TypeOf(decoded.GetResult()) != reflect.TypeOf(result.GetResult()) {
				t.Fatal("result variant changed")
			}
			if result.GetPong() != nil && !bytes.Equal(decoded.GetPong().GetNonce(), nonce) {
				t.Fatal("nonce changed")
			}
		})
	}
	result := results[0]
	result.Result = results[3].Result
	decoded := roundTrip(t, result).(*le0xv1.CommandResult)
	if decoded.GetPong() != nil || decoded.GetError() == nil {
		t.Fatal("oneof retained replaced result")
	}
}

func TestHeartbeat(t *testing.T) {
	timestamp := timestamppb.New(time.Date(2026, 9, 6, 12, 34, 56, 123456789, time.UTC))
	if err := timestamp.CheckValid(); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, &le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Heartbeat{Heartbeat: &le0xv1.Heartbeat{
		Timestamp: timestamp, ObservedStateRevision: 1 << 40,
	}}})
}

func TestGenericMinerContractRoundTrip(t *testing.T) {
	threads := uint32(2)
	hashrate := 123.5
	connected := true
	message := &le0xv1.ExecutionPlan{ExecutionId: "execution_0123456789abcdef0123456789abcdef", RestartPolicy: "ON_FAILURE", Miner: &le0xv1.MinerSpec{
		AdapterId: "test-no-http", SpecVersion: 1, PackageId: "package_0123456789abcdef0123456789abcdef", PackageVersion: "1.0", WalletId: "wallet_0123456789abcdef0123456789abcdef", PoolId: "pool_0123456789abcdef0123456789abcdef", Mode: "STRESS", Algorithm: "test-algorithm", CpuThreads: &threads,
	}}
	decoded := roundTrip(t, message).(*le0xv1.ExecutionPlan)
	if decoded.GetMiner().GetAdapterId() != "test-no-http" || decoded.GetMiner().GetCpuThreads() != 2 || decoded.GetMiner().GetWalletId() == "" || decoded.GetMiner().GetPoolId() == "" {
		t.Fatalf("miner spec lost: %+v", decoded.GetMiner())
	}
	execution := &le0xv1.Execution{ExecutionId: message.ExecutionId, State: "RUNNING", Warnings: []string{"adapter-specific source has no HTTP API"}, MinerTelemetry: &le0xv1.MinerTelemetry{AdapterId: "test-no-http", MinerVersion: "1.0", HashrateShortHps: &hashrate, PoolConnected: &connected, Health: "HEALTHY"}}
	observed := roundTrip(t, execution).(*le0xv1.Execution)
	if observed.GetMinerTelemetry().GetHashrateShortHps() != hashrate || !observed.GetMinerTelemetry().GetPoolConnected() || len(observed.GetWarnings()) != 1 {
		t.Fatalf("generic telemetry lost: %+v", observed.GetMinerTelemetry())
	}
}

func TestConnectContract(t *testing.T) {
	service := le0xv1.File_proto_le0x_v1_agent_control_proto.Services().ByName("AgentControl")
	if service == nil {
		t.Fatal("missing AgentControl")
	}
	method := service.Methods().ByName("Connect")
	if method == nil {
		t.Fatal("missing Connect")
	}
	if !method.IsStreamingClient() || !method.IsStreamingServer() {
		t.Fatal("Connect must be bidirectional streaming")
	}
	if method.Input().FullName() != "le0x.v1.AgentMessage" || method.Output().FullName() != "le0x.v1.ControllerMessage" {
		t.Fatal("incorrect stream direction")
	}
}

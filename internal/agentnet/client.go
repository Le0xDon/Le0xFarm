// Package agentnet contains the Agent's reconnecting development gRPC client.
// Plaintext is intentionally available only when InsecureDev is explicitly true.
package agentnet

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/agentpki"
	"github.com/le0xdon/le0xfarm/internal/agenttrust"
	"github.com/le0xdon/le0xfarm/internal/controllerpki"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/inventory"
	"github.com/le0xdon/le0xfarm/internal/minerruntime"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/protocol"
	"github.com/le0xdon/le0xfarm/internal/runtime/supervisor"
	"github.com/le0xdon/le0xfarm/internal/wiremap"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Config struct {
	Target      string
	InsecureDev bool
	// AllowRawExecution permits development-only executable/argv plans. It is
	// rejected unless InsecureDev is also true and is never enabled by the
	// production Agent entry point implicitly.
	AllowRawExecution bool
	AgentID           identity.AgentID
	HostID            identity.HostID
	Hostname          string
	Inventory         inventory.Source
	HeartbeatInterval time.Duration
	ReconnectInitial  time.Duration
	ReconnectMax      time.Duration
	Dialer            func(context.Context, string) (net.Conn, error)
	Output            *log.Logger
	EnrollmentToken   string
	TrustDir          string
	TLSFingerprint    string
	tlsConfig         *tls.Config
	Supervisor        *supervisor.Supervisor
	MinerRuntime      *minerruntime.Manager
}

type sessionError struct {
	err         error
	established bool
}

func (e sessionError) Error() string { return e.err.Error() }
func (e sessionError) Unwrap() error { return e.err }

// Run blocks until ctx is cancelled. It reconnects transient failures with bounded exponential backoff.
func Run(ctx context.Context, config Config) error {
	if config.AllowRawExecution && !config.InsecureDev {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "raw execution requires explicit insecure development mode"}
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = 10 * time.Second
	}
	if config.ReconnectInitial <= 0 {
		config.ReconnectInitial = time.Second
	}
	if config.ReconnectMax <= 0 {
		config.ReconnectMax = 30 * time.Second
	}
	if config.Output == nil {
		config.Output = log.Default()
	}
	if config.TrustDir == "" {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "Agent network mode requires a Controller trust directory"}
	}
	binding, trustErr := agenttrust.Load(config.TrustDir)
	if trustErr != nil && !errors.Is(trustErr, os.ErrNotExist) {
		return trustErr
	}
	if config.InsecureDev && errors.Is(trustErr, os.ErrNotExist) && config.EnrollmentToken == "" {
		return farmerr.Error{Code: farmerr.PAIRING_REQUIRED, HumanMessage: "Agent is not paired with a Controller", SuggestedFix: "Use --pair with a Controller enrollment token."}
	}
	if !config.InsecureDev {
		creds, err := agentpki.Load(config.TrustDir, binding, config.AgentID, config.HostID)
		if err != nil {
			if code, ok := farmerr.CodeOf(err); !ok || code != farmerr.TLS_CREDENTIALS_REQUIRED {
				return err
			}
			if config.EnrollmentToken == "" || config.TLSFingerprint == "" {
				return err
			}
			if err := bootstrap(ctx, config, binding, !errors.Is(trustErr, os.ErrNotExist)); err != nil {
				return err
			}
			binding, err = agenttrust.Load(config.TrustDir)
			if err != nil {
				return err
			}
			creds, err = agentpki.Load(config.TrustDir, binding, config.AgentID, config.HostID)
			if err != nil {
				return err
			}
		}
		config.EnrollmentToken = ""
		config.TLSFingerprint = ""
		config.tlsConfig = agentpki.TLSConfig(creds)
	}
	backoff := config.ReconnectInitial
	for {
		err := connectOnce(ctx, config)
		established := false
		var session sessionError
		if errors.As(err, &session) {
			established = session.established
			err = session.err
		}
		if ctx.Err() != nil {
			return nil
		}
		// A completed handshake/session resets the next reconnect delay
		// immediately, even when earlier dial attempts reached the maximum.
		if established {
			backoff = config.ReconnectInitial
		}
		if err != nil && nonTransient(err) {
			return err
		}
		if err != nil {
			config.Output.Printf("Controller connection lost: %v; reconnecting in %s", err, backoff)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if !established && backoff < config.ReconnectMax {
			backoff *= 2
			if backoff > config.ReconnectMax {
				backoff = config.ReconnectMax
			}
		}
	}
}

func connectOnce(ctx context.Context, config Config) error {
	if !config.InsecureDev {
		if err := verifyTLSEndpoint(ctx, config); err != nil {
			return err
		}
	}
	var opts []grpc.DialOption
	if config.Dialer != nil {
		opts = append(opts, grpc.WithContextDialer(config.Dialer))
	}
	if config.InsecureDev {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		if config.tlsConfig == nil {
			return farmerr.Error{Code: farmerr.TLS_CREDENTIALS_REQUIRED, HumanMessage: "Agent TLS credentials are required"}
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(config.tlsConfig)))
	}
	opts = append(opts, grpc.WithBlock())
	conn, err := grpc.DialContext(ctx, config.Target, opts...)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := le0xv1.NewAgentControlClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: &le0xv1.AgentHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: config.AgentID.String(), HostId: config.HostID.String(), Hostname: config.Hostname, EnrollmentToken: config.EnrollmentToken}}}); err != nil {
		return err
	}
	message, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := message.GetHello()
	if hello == nil {
		return errors.New("controller did not send ControllerHello")
	}
	if hello.ProtocolVersion != uint32(protocol.CurrentProtocolVersion) {
		return farmerr.Error{Code: farmerr.PROTOCOL_VERSION_MISMATCH, HumanMessage: fmt.Sprintf("controller protocol version %d is unsupported", hello.ProtocolVersion)}
	}
	if hello.SchemaVersion != uint32(protocol.CurrentSchemaVersion) {
		return farmerr.Error{Code: farmerr.SCHEMA_VERSION_MISMATCH, HumanMessage: fmt.Sprintf("controller schema version %d is unsupported", hello.SchemaVersion)}
	}
	if _, err := identity.ParseControllerID(hello.ControllerId); err != nil {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "ControllerHello contains invalid ControllerID"}
	}
	farmID, err := identity.ParseFarmID(hello.FarmId)
	if err != nil {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "ControllerHello contains invalid FarmID"}
	}
	controllerID, _ := identity.ParseControllerID(hello.ControllerId)
	if binding, err := agenttrust.Load(config.TrustDir); err == nil {
		if binding.ControllerID != controllerID || binding.FarmID != farmID {
			return farmerr.Error{Code: farmerr.CONTROLLER_IDENTITY_MISMATCH, HumanMessage: "Controller identity differs from trusted binding"}
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := agenttrust.Save(config.TrustDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}); err != nil {
			return err
		}
	} else {
		return err
	}
	config.Output.Printf("Connected to Controller %s (Farm %s)", hello.ControllerId, hello.FarmId)
	heartbeats := make(chan error, 1)
	var sendMu sync.Mutex
	go func() { heartbeats <- heartbeatLoop(ctx, stream, config.HeartbeatInterval, &sendMu) }()
	for {
		message, err := stream.Recv()
		if err != nil {
			return sessionError{err: err, established: true}
		}
		command := message.GetCommand()
		if command == nil {
			continue
		}
		if err := handleCommand(stream, command, config, &sendMu); err != nil {
			return err
		}
		select {
		case err := <-heartbeats:
			return sessionError{err: err, established: true}
		default:
		}
	}
}

func bootstrap(ctx context.Context, config Config, binding agenttrust.Binding, hasBinding bool) error {
	want, err := parseFingerprint(config.TLSFingerprint)
	if err != nil {
		return farmerr.Error{Code: farmerr.TLS_FINGERPRINT_MISMATCH, HumanMessage: "Invalid TLS fingerprint format"}
	}
	var controllerID identity.ControllerID
	var farmID identity.FarmID
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return farmerr.Error{Code: farmerr.TLS_FINGERPRINT_MISMATCH, HumanMessage: "Controller did not present a certificate"}
		}
		got := sha256.Sum256(rawCerts[0])
		if subtle.ConstantTimeCompare(got[:], want) != 1 {
			return farmerr.Error{Code: farmerr.TLS_FINGERPRINT_MISMATCH, HumanMessage: "Controller TLS certificate fingerprint does not match"}
		}
		cert, e := x509.ParseCertificate(rawCerts[0])
		if e != nil {
			return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Invalid Controller certificate"}
		}
		now := time.Now()
		if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			return farmerr.Error{Code: farmerr.CERTIFICATE_EXPIRED, HumanMessage: "Controller certificate is not currently valid"}
		}
		controllerID, farmID, e = controllerpki.ParseControllerIdentity(cert)
		if e != nil {
			return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Controller certificate identity is invalid"}
		}
		if hasBinding && (binding.ControllerID != controllerID || binding.FarmID != farmID) {
			return farmerr.Error{Code: farmerr.CONTROLLER_IDENTITY_MISMATCH, HumanMessage: "Controller certificate differs from trusted binding"}
		}
		return nil
	}}
	if err := verifyBootstrapEndpoint(ctx, config, tlsConfig); err != nil {
		return err
	}
	var opts []grpc.DialOption
	if config.Dialer != nil {
		opts = append(opts, grpc.WithContextDialer(config.Dialer))
	}
	opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithBlock())
	conn, err := grpc.DialContext(ctx, config.Target, opts...)
	if err != nil {
		return classifyBootstrapTLS(err)
	}
	defer conn.Close()
	// TLS verification has populated the certificate identities before the stream starts.
	enrollment, err := agentpki.NewEnrollment(config.AgentID, config.HostID, farmID)
	if err != nil {
		return err
	}
	stream, err := le0xv1.NewAgentControlClient(conn).Connect(ctx)
	if err != nil {
		return err
	}
	hello := &le0xv1.AgentHello{ProtocolVersion: uint32(protocol.CurrentProtocolVersion), SchemaVersion: uint32(protocol.CurrentSchemaVersion), AgentId: config.AgentID.String(), HostId: config.HostID.String(), Hostname: config.Hostname, EnrollmentToken: config.EnrollmentToken, CertificateRequestDer: enrollment.CSRDER}
	if err = stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Hello{Hello: hello}}); err != nil {
		return err
	}
	message, err := stream.Recv()
	if err != nil {
		return err
	}
	response := message.GetHello()
	if response == nil || len(response.AgentCertificateDer) == 0 || len(response.FarmCaCertificateDer) == 0 {
		return farmerr.Error{Code: farmerr.TLS_CREDENTIALS_REQUIRED, HumanMessage: "Controller did not return enrollment certificates"}
	}
	if response.ControllerId != controllerID.String() || response.FarmId != farmID.String() {
		return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "ControllerHello identity does not match TLS certificate"}
	}
	if !hasBinding {
		if err = agenttrust.Save(config.TrustDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}); err != nil {
			return err
		}
		binding = agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}
	}
	if err = agentpki.Save(config.TrustDir, enrollment, response.AgentCertificateDer, response.FarmCaCertificateDer, binding, config.AgentID, config.HostID); err != nil {
		return err
	}
	config.Output.Printf("TLS enrollment complete; reconnecting with mutual TLS")
	return nil
}

func parseFingerprint(value string) ([]byte, error) {
	if !strings.HasPrefix(strings.ToUpper(value), "SHA256:") {
		return nil, errors.New("missing SHA256 prefix")
	}
	raw := strings.ReplaceAll(value[len("SHA256:"):], ":", "")
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("invalid SHA-256 fingerprint")
	}
	return decoded, nil
}
func classifyBootstrapTLS(err error) error {
	var typed farmerr.Error
	if errors.As(err, &typed) {
		return typed
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		if invalid.Reason == x509.Expired {
			return farmerr.Error{Code: farmerr.CERTIFICATE_EXPIRED, HumanMessage: "TLS certificate is not currently valid"}
		}
		return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "TLS certificate validation failed"}
	}
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Controller certificate is not signed by the trusted Farm CA"}
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Controller certificate server identity is invalid"}
	}
	var roots x509.SystemRootsError
	if errors.As(err, &roots) {
		return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Trusted Farm CA roots are unavailable"}
	}
	return err
}

func verifyBootstrapEndpoint(ctx context.Context, config Config, tlsConfig *tls.Config) error {
	var raw net.Conn
	var err error
	if config.Dialer != nil {
		raw, err = config.Dialer(ctx, config.Target)
	} else {
		var dialer net.Dialer
		raw, err = dialer.DialContext(ctx, "tcp", config.Target)
	}
	if err != nil {
		return err
	}
	conn := tls.Client(raw, tlsConfig.Clone())
	defer conn.Close()
	if err := conn.HandshakeContext(ctx); err != nil {
		return classifyBootstrapTLS(err)
	}
	return nil
}

func verifyTLSEndpoint(ctx context.Context, config Config) error {
	if config.tlsConfig == nil {
		return farmerr.Error{Code: farmerr.TLS_CREDENTIALS_REQUIRED, HumanMessage: "Agent TLS credentials are required"}
	}
	return verifyBootstrapEndpoint(ctx, config, config.tlsConfig)
}

func heartbeatLoop(ctx context.Context, stream le0xv1.AgentControl_ConnectClient, interval time.Duration, sendMu *sync.Mutex) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			sendMu.Lock()
			if err := stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_Heartbeat{Heartbeat: &le0xv1.Heartbeat{Timestamp: timestamppb.New(now), ObservedStateRevision: 0}}}); err != nil {
				sendMu.Unlock()
				return err
			}
			sendMu.Unlock()
		}
	}
}

func handleCommand(stream le0xv1.AgentControl_ConnectClient, command *le0xv1.CommandEnvelope, config Config, sendMu *sync.Mutex) error {
	result := &le0xv1.CommandResult{CommandId: command.CommandId}
	switch {
	case command.GetPing() != nil:
		result.Result = &le0xv1.CommandResult_Pong{Pong: &le0xv1.Pong{Nonce: append([]byte(nil), command.GetPing().Nonce...)}}
	case command.GetGetStatus() != nil:
		state := model.AgentStateIdle
		if config.MinerRuntime != nil {
			state = config.MinerRuntime.OverallStatus()
		}
		result.Result = &le0xv1.CommandResult_Status{Status: &le0xv1.Status{AgentState: string(state)}}
	case command.GetGetInventory() != nil:
		facts, _ := config.Inventory.Discover(config.HostID)
		if config.MinerRuntime != nil {
			config.MinerRuntime.SetInventory(facts)
		}
		result.Result = &le0xv1.CommandResult_Inventory{Inventory: wiremap.Inventory(facts)}
	case command.GetStartExecution() != nil:
		if config.Supervisor == nil && config.MinerRuntime == nil {
			result.Result = wireError(farmerr.CONFIG_CONFLICT, "runtime supervisor is unavailable")
			break
		}
		plan, err := parsePlanForHost(command.GetStartExecution().GetPlan(), config.HostID)
		if err != nil {
			result.Result = errorResult(err)
			break
		}
		if plan.Miner == nil {
			if !config.InsecureDev || !config.AllowRawExecution {
				result.Result = wireError(farmerr.PERMISSION_DENIED, "raw executable plans are disabled on this Agent")
				break
			}
			if config.Supervisor == nil {
				result.Result = wireError(farmerr.CONFIG_CONFLICT, "runtime supervisor is unavailable")
				break
			}
			snap, msg, err := config.Supervisor.Start(plan)
			if err != nil {
				result.Result = errorResult(err)
			} else {
				result.Result = &le0xv1.CommandResult_Execution{Execution: &le0xv1.ExecutionResult{Execution: wireExecution(snap), Message: msg}}
			}
			break
		}
		if config.MinerRuntime != nil {
			// Re-discover at the execution boundary so a Controller plan bound to
			// an earlier ordinal/physical device cannot slip through after a hot
			// replacement. Manager validates before preparation and again under a
			// final pre-exec inventory lease after preparation completes.
			facts, _ := config.Inventory.Discover(config.HostID)
			config.MinerRuntime.SetInventory(facts)
			observation, msg, err := config.MinerRuntime.Start(stream.Context(), plan)
			if err != nil {
				result.Result = errorResult(err)
			} else {
				result.Result = &le0xv1.CommandResult_Execution{Execution: &le0xv1.ExecutionResult{Execution: wireObservation(observation), Message: msg}}
			}
			break
		}
		result.Result = wireError(farmerr.CONFIG_CONFLICT, "miner runtime is unavailable")
	case command.GetStopExecution() != nil:
		if config.Supervisor == nil && config.MinerRuntime == nil {
			result.Result = wireError(farmerr.CONFIG_CONFLICT, "runtime supervisor is unavailable")
			break
		}
		id, err := identity.ParseExecutionID(command.GetStopExecution().ExecutionId)
		if err != nil {
			result.Result = wireError(farmerr.CONFIG_CONFLICT, "invalid ExecutionID")
			break
		}
		if config.MinerRuntime != nil {
			observation, msg, err := config.MinerRuntime.Stop(id)
			if err != nil {
				result.Result = errorResult(err)
			} else {
				result.Result = &le0xv1.CommandResult_Execution{Execution: &le0xv1.ExecutionResult{Execution: wireObservation(observation), Message: msg}}
			}
			break
		}
		snap, msg, err := config.Supervisor.Stop(id)
		if err != nil {
			result.Result = errorResult(err)
		} else {
			result.Result = &le0xv1.CommandResult_Execution{Execution: &le0xv1.ExecutionResult{Execution: wireExecution(snap), Message: msg}}
		}
	case command.GetRestartExecution() != nil:
		if config.Supervisor == nil && config.MinerRuntime == nil {
			result.Result = wireError(farmerr.CONFIG_CONFLICT, "runtime supervisor is unavailable")
			break
		}
		id, err := identity.ParseExecutionID(command.GetRestartExecution().ExecutionId)
		if err != nil {
			result.Result = wireError(farmerr.CONFIG_CONFLICT, "invalid ExecutionID")
			break
		}
		if config.MinerRuntime != nil {
			observation, msg, err := config.MinerRuntime.Restart(id)
			if err != nil {
				result.Result = errorResult(err)
			} else {
				result.Result = &le0xv1.CommandResult_Execution{Execution: &le0xv1.ExecutionResult{Execution: wireObservation(observation), Message: msg}}
			}
			break
		}
		snap, msg, err := config.Supervisor.Restart(id)
		if err != nil {
			result.Result = errorResult(err)
		} else {
			result.Result = &le0xv1.CommandResult_Execution{Execution: &le0xv1.ExecutionResult{Execution: wireExecution(snap), Message: msg}}
		}
	case command.GetGetExecutions() != nil:
		if config.Supervisor == nil && config.MinerRuntime == nil {
			result.Result = wireError(farmerr.CONFIG_CONFLICT, "runtime supervisor is unavailable")
			break
		}
		wire := make([]*le0xv1.Execution, 0)
		if config.MinerRuntime != nil {
			for _, item := range config.MinerRuntime.List() {
				wire = append(wire, wireObservation(item))
			}
		} else {
			for _, item := range config.Supervisor.List() {
				wire = append(wire, wireExecution(item))
			}
		}
		result.Result = &le0xv1.CommandResult_Executions{Executions: &le0xv1.Executions{Executions: wire}}
	default:
		result.Result = &le0xv1.CommandResult_Error{Error: &le0xv1.TypedError{Code: "MISSING_COMMAND", HumanMessage: "Command variant is missing"}}
	}
	sendMu.Lock()
	defer sendMu.Unlock()
	return stream.Send(&le0xv1.AgentMessage{Payload: &le0xv1.AgentMessage_CommandResult{CommandResult: result}})
}

func parsePlan(in *le0xv1.ExecutionPlan) (model.ExecutionPlan, error) {
	return parsePlanForHost(in, identity.HostID{})
}

func parsePlanForHost(in *le0xv1.ExecutionPlan, expectedHost identity.HostID) (model.ExecutionPlan, error) {
	if in == nil {
		return model.ExecutionPlan{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "ExecutionPlan is required"}
	}
	id, err := identity.ParseExecutionID(in.ExecutionId)
	if err != nil {
		return model.ExecutionPlan{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid ExecutionID"}
	}
	ownership, err := parseOwnership(in.GetOwnership())
	if err != nil {
		return model.ExecutionPlan{}, err
	}
	if expectedHost.Validate() == nil && ownership.HostID != expectedHost {
		return model.ExecutionPlan{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "ExecutionPlan target HostID does not match this Agent"}
	}
	plan := model.ExecutionPlan{SchemaVersion: protocol.CurrentSchemaVersion, ExecutionID: id, Ownership: ownership, HostID: ownership.HostID,
		DeviceIDs: append([]identity.DeviceID(nil), ownership.DeviceIDs...), Executable: in.Executable, Args: append([]string(nil), in.Args...), Environment: maps.Clone(in.Environment), WorkingDirectory: in.WorkingDirectory, RestartPolicy: model.RestartPolicy(in.RestartPolicy)}
	if wire := in.GetMiner(); wire != nil {
		packageID, err := identity.ParsePackageID(wire.PackageId)
		if err != nil {
			return model.ExecutionPlan{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid miner PackageID"}
		}
		devices := make([]identity.DeviceID, 0, len(wire.GpuDeviceIds))
		for _, value := range wire.GpuDeviceIds {
			device, err := identity.ParseDeviceID(value)
			if err != nil {
				return model.ExecutionPlan{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid miner DeviceID"}
			}
			devices = append(devices, device)
		}
		assignments := make([]model.GPUAssignment, 0, len(wire.GpuAssignments))
		for _, value := range wire.GpuAssignments {
			if value == nil {
				return model.ExecutionPlan{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid nil GPU assignment"}
			}
			device, err := identity.ParseDeviceID(value.DeviceId)
			if err != nil {
				return model.ExecutionPlan{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid GPU assignment DeviceID"}
			}
			assignments = append(assignments, model.GPUAssignment{DeviceID: device, HardwareIdentity: value.HardwareIdentity, RuntimeSelector: value.RuntimeSelector})
		}
		var endpoint *model.MiningEndpoint
		if value := wire.GetEndpoint(); value != nil {
			endpoint = &model.MiningEndpoint{Address: value.Address, TLS: value.Tls, User: value.User, Password: value.Password, Worker: value.Worker}
		}
		plan.Miner = &model.MinerSpec{AdapterID: wire.AdapterId, SpecVersion: wire.SpecVersion, PackageID: packageID, PackageVersion: wire.PackageVersion, Mode: model.MinerMode(wire.Mode), Coin: wire.Coin, Algorithm: wire.Algorithm, Endpoint: endpoint, CPUThreads: wire.CpuThreads, GPUDeviceIDs: devices, GPUAssignments: assignments, HugePages: wire.HugePages, MSR: wire.Msr, Options: maps.Clone(wire.Options)}
		if !slices.Equal(devices, ownership.DeviceIDs) {
			return model.ExecutionPlan{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "miner DeviceIDs do not match ownership ResourceClaim"}
		}
	}
	return plan, nil
}

func parseOwnership(in *le0xv1.WorkloadOwnership) (model.WorkloadOwnership, error) {
	if in == nil || in.GetResourceClaim() == nil {
		return model.WorkloadOwnership{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "workload ownership metadata is required"}
	}
	workloadID, err := identity.ParseWorkloadID(in.WorkloadId)
	if err != nil {
		return model.WorkloadOwnership{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid WorkloadID"}
	}
	hostID, err := identity.ParseHostID(in.HostId)
	if err != nil {
		return model.WorkloadOwnership{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid ownership HostID"}
	}
	devices := make([]identity.DeviceID, 0, len(in.ResourceClaim.DeviceIds))
	for _, raw := range in.ResourceClaim.DeviceIds {
		deviceID, err := identity.ParseDeviceID(raw)
		if err != nil {
			return model.WorkloadOwnership{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid ownership ResourceClaim DeviceID"}
		}
		devices = append(devices, deviceID)
	}
	ownership := model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: in.DesiredGeneration, ResolvedHash: in.ResolvedHash, HostID: hostID, CPU: in.ResourceClaim.Cpu, DeviceIDs: devices}
	if err := ownership.Validate(); err != nil {
		return model.WorkloadOwnership{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid workload ownership metadata", Details: map[string]string{"reason": err.Error()}}
	}
	return ownership, nil
}
func wireExecution(s supervisor.Snapshot) *le0xv1.Execution {
	out := &le0xv1.Execution{ExecutionId: s.ExecutionID.String(), State: string(s.State), Pid: int64(s.PID), RestartCount: s.RestartCount, LastError: s.LastError}
	if s.Ownership.Validate() == nil {
		out.Ownership = wireOwnership(s.Ownership)
	}
	if !s.StartedAt.IsZero() {
		out.StartedAt = timestamppb.New(s.StartedAt)
	}
	if s.ExitCode != nil {
		out.HasExitCode = true
		out.ExitCode = int32(*s.ExitCode)
	}
	return out
}

func wireOwnership(ownership model.WorkloadOwnership) *le0xv1.WorkloadOwnership {
	devices := make([]string, len(ownership.DeviceIDs))
	for i, deviceID := range ownership.DeviceIDs {
		devices[i] = deviceID.String()
	}
	return &le0xv1.WorkloadOwnership{WorkloadId: ownership.WorkloadID.String(), DesiredGeneration: ownership.DesiredGeneration, ResolvedHash: ownership.ResolvedHash, HostId: ownership.HostID.String(), ResourceClaim: &le0xv1.ResourceClaim{Cpu: ownership.CPU, DeviceIds: devices}}
}

func wireObservation(observation minerruntime.Observation) *le0xv1.Execution {
	out := wireExecution(observation.Process)
	out.Warnings = append([]string(nil), observation.Warnings...)
	if telemetry := observation.Telemetry; telemetry != nil {
		wire := &le0xv1.MinerTelemetry{AdapterId: telemetry.AdapterID, MinerVersion: telemetry.MinerVersion, Algorithm: telemetry.Algorithm, HashrateShortHps: telemetry.HashrateShortHPS, HashrateMediumHps: telemetry.HashrateMediumHPS, HashrateLongHps: telemetry.HashrateLongHPS, HighestHashrateHps: telemetry.HighestHashrateHPS, AcceptedShares: telemetry.AcceptedShares, RejectedShares: telemetry.RejectedShares, StaleShares: telemetry.StaleShares, TotalResults: telemetry.TotalResults, PoolConnected: telemetry.PoolConnected, PoolLatencyMs: telemetry.PoolLatencyMS, UptimeSeconds: telemetry.UptimeSeconds, HugePagesAvailable: telemetry.HugePagesAvailable, HugePagesPercent: telemetry.HugePagesPercent, MsrAvailable: telemetry.MSRAvailable, AgeMilliseconds: uint64(max(telemetry.Age.Milliseconds(), 0)), Health: string(telemetry.Health), ErrorCode: string(telemetry.ErrorCode), Message: telemetry.Message}
		if !telemetry.CollectedAt.IsZero() {
			wire.CollectedAt = timestamppb.New(telemetry.CollectedAt)
		}
		for _, device := range telemetry.PerDevice {
			wire.PerDevice = append(wire.PerDevice, &le0xv1.DeviceHashrate{DeviceId: device.DeviceID.String(), HashrateHps: device.HashrateHPS})
		}
		out.MinerTelemetry = wire
	}
	return out
}
func wireError(code farmerr.Code, msg string) *le0xv1.CommandResult_Error {
	return &le0xv1.CommandResult_Error{Error: &le0xv1.TypedError{Code: string(code), HumanMessage: msg}}
}
func errorResult(err error) *le0xv1.CommandResult_Error {
	var typed farmerr.Error
	if errors.As(err, &typed) {
		return &le0xv1.CommandResult_Error{Error: &le0xv1.TypedError{
			Code:         string(typed.Code),
			HumanMessage: typed.HumanMessage,
			Details:      maps.Clone(typed.Details),
			SuggestedFix: typed.SuggestedFix,
			LogsRef:      typed.LogsRef,
		}}
	}
	return wireError(farmerr.INTERNAL_ERROR, err.Error())
}

func nonTransient(err error) bool {
	if err == nil {
		return false
	}
	if code, ok := farmerr.CodeOf(err); ok && (code == farmerr.PROTOCOL_VERSION_MISMATCH || code == farmerr.SCHEMA_VERSION_MISMATCH) {
		return true
	}
	if code, ok := farmerr.CodeOf(err); ok && (code == farmerr.PAIRING_REQUIRED || code == farmerr.PAIRING_TOKEN_INVALID || code == farmerr.PAIRING_TOKEN_EXPIRED || code == farmerr.CONTROLLER_IDENTITY_MISMATCH || code == farmerr.CONFIG_CONFLICT) {
		return true
	}
	if code, ok := farmerr.CodeOf(err); ok && (code == farmerr.TLS_CREDENTIALS_REQUIRED || code == farmerr.TLS_FINGERPRINT_MISMATCH || code == farmerr.TLS_IDENTITY_MISMATCH || code == farmerr.CERTIFICATE_EXPIRED) {
		return true
	}
	if status.Code(err) == codes.InvalidArgument {
		return true
	}
	return false
}

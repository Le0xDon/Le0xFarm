//go:build linux

package controlleradmin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerdb"
	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmconfig"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/packagecatalog"
)

type testController struct{ service *farmconfig.Service }

func (c testController) SetMaintenanceHold(ctx context.Context, host identity.HostID, revision uint64, active bool, reason string) (farmmodel.MaintenanceHold, error) {
	return c.service.SetMaintenanceHold(ctx, host, revision, active, reason)
}
func (c testController) DeleteDesiredWorkload(ctx context.Context, id identity.WorkloadID, revision uint64) error {
	return c.service.DeleteDesiredWorkload(ctx, id, revision)
}

func TestOperatorRPCSequenceUsesFarmConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := filepath.Join(t.TempDir(), "controller")
	db, err := controllerdb.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	catalog, _ := packagecatalog.Builtin()
	config, err := farmconfig.New(db, catalog, farmconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	observed := controllerstate.New()
	host, _ := identity.ParseHostID("host_11111111111111111111111111111111")
	agent, _ := identity.ParseAgentID("agent_11111111111111111111111111111111")
	if !observed.Connect(agent, host, 1) {
		t.Fatal("connect")
	}
	observed.SetExecutions(host, 1, nil, time.Now(), 0)
	observed.SetInventory(host, 1, model.Inventory{Host: model.Host{HostID: host}})
	observed.SetUnmanagedProcesses(host, 1, nil, time.Now())
	observed.SetAgentState(host, 1, model.AgentStateIdle)
	observed.MarkReady(host, 1)
	listener, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(SocketPath(dir)); err != nil || info.Mode().Perm() != 0600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("operator socket mode=%v err=%v", func() os.FileMode {
			if info != nil {
				return info.Mode()
			}
			return 0
		}(), err)
	}
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, &Service{Backend: config, Controller: testController{config}, Observed: observed})
	}()
	call := func(request Request, target any) []byte {
		t.Helper()
		data, err := Call(context.Background(), "unix", SocketPath(dir), request)
		if err != nil {
			t.Fatal(err)
		}
		if target != nil && json.Unmarshal(data, target) != nil {
			t.Fatalf("invalid response: %s", data)
		}
		return data
	}
	var hosts []controllerstate.HostObservation
	call(Request{Action: "hosts-list"}, &hosts)
	if len(hosts) != 1 || !hosts[0].Ready {
		t.Fatalf("hosts=%+v", hosts)
	}
	var pool farmmodel.Pool
	call(Request{Action: "pool-create", Name: "acceptance", Address: "pool.example:443", TLS: true}, &pool)
	var wallet farmmodel.WalletRef
	call(Request{Action: "wallet-create", Name: "payout", Coin: "XMR", Address: "public-test-address"}, &wallet)
	manifestID := "package_5f5d8ae2b63ce1001aa0a3b8a2a9b29a"
	var profile farmmodel.MiningProfile
	call(Request{Action: "profile-create", Name: "xmr", AdapterID: "xmrig", PackageID: manifestID, PackageVersion: "6.26.0", Mode: "MINING", Coin: "XMR", Algorithm: "rx/0", PoolID: pool.PoolID.String(), WalletID: wallet.WalletID.String(), UserTemplate: "${wallet}", WorkerPlacement: "NONE", CPUThreads: 2}, &profile)
	var workload farmmodel.DesiredWorkload
	call(Request{Action: "workload-create", Name: "cpu", HostID: host.String(), ProfileID: profile.ProfileID.String(), RunState: "STOPPED", CPU: true}, &workload)
	call(Request{Action: "workload-set", WorkloadID: workload.WorkloadID.String(), RunState: "RUNNING"}, &workload)
	if workload.RunState != farmmodel.DesiredRunning {
		t.Fatalf("state=%s", workload.RunState)
	}
	snapshot, err := config.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	useful := &model.UsefulWorkEvidence{Provider: "xmrig", Availability: model.TelemetryAvailable, UsefulWork: model.UsefulWorkConfirmed, CollectedAt: time.Now().UTC(), FreshFor: time.Minute}
	observed.SetExecutions(host, 1, []model.ExecutionObservation{{ExecutionID: snapshot.ExecutionID, Ownership: &snapshot.Plan.Ownership, Status: model.ExecutionRunning, PID: 123, UsefulWork: useful}}, time.Now(), 1)
	var status struct {
		Observed controllerstate.HostObservation `json:"observed"`
	}
	call(Request{Action: "status", WorkloadID: workload.WorkloadID.String()}, &status)
	if len(status.Observed.Executions) != 1 || status.Observed.Executions[0].UsefulWork == nil || status.Observed.Executions[0].UsefulWork.UsefulWork != model.UsefulWorkConfirmed {
		t.Fatalf("status does not expose useful-work evidence: %+v", status)
	}
	var hold farmmodel.MaintenanceHold
	call(Request{Action: "hold-set", HostID: host.String(), Reason: "acceptance"}, &hold)
	if !hold.Active {
		t.Fatal("hold not active")
	}
	call(Request{Action: "hold-clear", HostID: host.String()}, &hold)
	if hold.Active {
		t.Fatal("hold not cleared")
	}
	call(Request{Action: "workload-set", WorkloadID: workload.WorkloadID.String(), RunState: "STOPPED"}, &workload)
	var incidents []farmmodel.Incident
	call(Request{Action: "incidents-active"}, &incidents)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestListenRefusesNonSocketEntry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(SocketPath(dir), []byte("do not replace"), 0600); err != nil {
		t.Fatal(err)
	}
	if listener, err := Listen(dir); err == nil {
		listener.Close()
		t.Fatal("non-socket operator entry was replaced")
	}
	if data, err := os.ReadFile(SocketPath(dir)); err != nil || string(data) != "do not replace" {
		t.Fatalf("existing entry changed: %q %v", data, err)
	}
}

func TestOversizedRequestIsRejectedBeforeDecode(t *testing.T) {
	dir := t.TempDir()
	listener, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, &Service{}) }()
	connection, err := net.Dial("unix", SocketPath(dir))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(maxRequestBytes+1))
	if _, err := connection.Write(header[:]); err != nil {
		connection.Close()
		cancel()
		t.Fatal(err)
	}
	data, err := readFrame(connection, maxResponseBytes+maxRequestBytes)
	connection.Close()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(data, &response); err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.ErrorCode != "CONFIG_CONFLICT" || response.Error == "" {
		cancel()
		t.Fatalf("response=%+v", response)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnknownAndTrailingRequestContentIsRejected(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
	}{
		{name: "unknown field", data: `{"Action":"hosts-list","Unexpected":true}`},
		{name: "trailing value", data: `{"Action":"hosts-list"}{"Action":"hosts-list"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				serveConnection(server, &Service{})
				server.Close()
			}()
			if err := writeFrame(client, []byte(test.data)); err != nil {
				t.Fatal(err)
			}
			data, err := readFrame(client, maxResponseBytes+maxRequestBytes)
			client.Close()
			if err != nil {
				t.Fatal(err)
			}
			<-done
			var response Response
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatal(err)
			}
			if response.ErrorCode != "CONFIG_CONFLICT" || response.Error == "" {
				t.Fatalf("response=%+v", response)
			}
		})
	}
}

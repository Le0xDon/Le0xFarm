package controlleradmin

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmconfig"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const SocketName = "operator.sock"

const (
	maxRequestBytes  = 64 << 10
	maxResponseBytes = 1 << 20
)

type Request struct {
	Action, ID, Name, Address, Coin, AdapterID, PackageID, PackageVersion  string
	Mode, Algorithm, PoolID, WalletID, ProfileID, HostID, WorkloadID       string
	RunState, Worker, UserTemplate, WorkerPlacement, Reason, IncidentState string
	TLS, CPU                                                               bool
	CPUThreads                                                             uint32
	DeviceIDs                                                              []string
	Limit                                                                  int
}

type Response struct {
	JSON      json.RawMessage
	ErrorCode string
	Error     string
}

type Backend interface {
	farmconfig.Pools
	farmconfig.WalletRefs
	farmconfig.MiningProfiles
	farmconfig.DesiredWorkloads
	farmconfig.MaintenanceHolds
	farmconfig.Incidents
}

type Controller interface {
	SetMaintenanceHold(context.Context, identity.HostID, uint64, bool, string) (farmmodel.MaintenanceHold, error)
	DeleteDesiredWorkload(context.Context, identity.WorkloadID, uint64) error
}

type Service struct {
	Backend    Backend
	Controller Controller
	Observed   *controllerstate.Store
}

func (service *Service) Execute(request Request, response *Response) error {
	if service.Backend == nil || service.Controller == nil || service.Observed == nil {
		return errors.New("operator service is not configured")
	}
	value, err := service.execute(context.Background(), request)
	if err != nil {
		code, typed := farmerr.CodeOf(err)
		if !typed {
			code = farmerr.INTERNAL_ERROR
		}
		response.ErrorCode = string(code)
		response.Error = err.Error()
		return nil
	}
	data, err := json.MarshalIndent(value, "", "  ")
	response.JSON = data
	if err == nil && len(response.JSON) > maxResponseBytes {
		response.JSON = nil
		response.ErrorCode = string(farmerr.CONFIG_CONFLICT)
		response.Error = "operator response exceeds the bounded local output limit"
	}
	return err
}

func (service *Service) execute(ctx context.Context, r Request) (any, error) {
	switch r.Action {
	case "hosts-list":
		return service.Observed.List(), nil
	case "host-show":
		id, err := identity.ParseHostID(r.HostID)
		if err != nil {
			return nil, invalid("host ID", err)
		}
		value, ok := service.Observed.Get(id)
		if !ok {
			return nil, notFound("Host")
		}
		return value, nil
	case "pool-create":
		kind := farmmodel.PoolAuthNone
		return service.Backend.CreatePool(ctx, farmmodel.PoolContent{Name: r.Name, Address: r.Address, TLS: r.TLS, Auth: farmmodel.PoolAuth{Kind: kind}})
	case "pool-list":
		return service.Backend.ListPools(ctx)
	case "pool-show":
		id, err := identity.ParsePoolID(first(r.PoolID, r.ID))
		if err != nil {
			return nil, invalid("pool ID", err)
		}
		return service.Backend.GetPool(ctx, id)
	case "wallet-create":
		return service.Backend.CreateWalletRef(ctx, farmmodel.WalletRefContent{Name: r.Name, Coin: r.Coin, Address: r.Address})
	case "wallet-list":
		return service.Backend.ListWalletRefs(ctx)
	case "wallet-show":
		id, err := identity.ParseWalletID(first(r.WalletID, r.ID))
		if err != nil {
			return nil, invalid("wallet ID", err)
		}
		return service.Backend.GetWalletRef(ctx, id)
	case "profile-create":
		packageID, err := identity.ParsePackageID(r.PackageID)
		if err != nil {
			return nil, invalid("package ID", err)
		}
		poolID, err := identity.ParsePoolID(r.PoolID)
		if err != nil {
			return nil, invalid("pool ID", err)
		}
		walletID, err := identity.ParseWalletID(r.WalletID)
		if err != nil {
			return nil, invalid("wallet ID", err)
		}
		placement := farmmodel.WorkerPlacement(r.WorkerPlacement)
		if placement == "" {
			placement = farmmodel.WorkerNone
		}
		mode := farmmodel.ProfileMode(r.Mode)
		if mode == "" {
			mode = farmmodel.ProfileModeMining
		}
		var threads *uint32
		if r.CPUThreads > 0 {
			threads = &r.CPUThreads
		}
		return service.Backend.CreateMiningProfile(ctx, farmmodel.MiningProfileContent{Name: r.Name, AdapterID: r.AdapterID, Package: farmmodel.PackageRef{PackageID: packageID, Version: r.PackageVersion}, Mode: mode, Coin: r.Coin, Algorithm: r.Algorithm, PoolID: poolID, WalletID: walletID, LoginPolicy: farmmodel.LoginPolicy{UserTemplate: r.UserTemplate, WorkerPlacement: placement}, CPUThreads: threads})
	case "profile-list":
		return service.Backend.ListMiningProfiles(ctx)
	case "profile-show":
		id, err := identity.ParseProfileID(first(r.ProfileID, r.ID))
		if err != nil {
			return nil, invalid("profile ID", err)
		}
		return service.Backend.GetMiningProfile(ctx, id)
	case "workload-create":
		hostID, err := identity.ParseHostID(r.HostID)
		if err != nil {
			return nil, invalid("host ID", err)
		}
		profileID, err := identity.ParseProfileID(r.ProfileID)
		if err != nil {
			return nil, invalid("profile ID", err)
		}
		claim, err := resourceClaim(r)
		if err != nil {
			return nil, err
		}
		state := farmmodel.DesiredRunState(r.RunState)
		if state == "" {
			state = farmmodel.DesiredStopped
		}
		return service.Backend.CreateDesiredWorkload(ctx, farmmodel.DesiredWorkloadContent{Name: r.Name, HostID: hostID, ProfileID: profileID, RunState: state, Worker: r.Worker, Resources: claim})
	case "workload-list":
		return service.Backend.ListDesiredWorkloads(ctx)
	case "workload-show":
		id, err := identity.ParseWorkloadID(first(r.WorkloadID, r.ID))
		if err != nil {
			return nil, invalid("workload ID", err)
		}
		return service.workloadStatus(ctx, id)
	case "workload-set":
		id, err := identity.ParseWorkloadID(first(r.WorkloadID, r.ID))
		if err != nil {
			return nil, invalid("workload ID", err)
		}
		current, err := service.Backend.GetDesiredWorkload(ctx, id)
		if err != nil {
			return nil, err
		}
		state := farmmodel.DesiredRunState(r.RunState)
		if state != farmmodel.DesiredRunning && state != farmmodel.DesiredStopped {
			return nil, invalid("run state", nil)
		}
		content := current.DesiredWorkloadContent
		content.RunState = state
		return service.Backend.UpdateDesiredWorkload(ctx, id, current.Meta.Revision, content)
	case "workload-delete":
		id, err := identity.ParseWorkloadID(first(r.WorkloadID, r.ID))
		if err != nil {
			return nil, invalid("workload ID", err)
		}
		current, err := service.Backend.GetDesiredWorkload(ctx, id)
		if err != nil {
			return nil, err
		}
		if err = service.Controller.DeleteDesiredWorkload(ctx, id, current.Meta.Revision); err != nil {
			return nil, err
		}
		return map[string]string{"deleted": id.String()}, nil
	case "hold-set", "hold-clear":
		id, err := identity.ParseHostID(r.HostID)
		if err != nil {
			return nil, invalid("host ID", err)
		}
		current, exists, err := service.Backend.GetMaintenanceHold(ctx, id)
		if err != nil {
			return nil, err
		}
		revision := uint64(0)
		if exists {
			revision = current.Revision
		}
		return service.Controller.SetMaintenanceHold(ctx, id, revision, r.Action == "hold-set", r.Reason)
	case "hold-list":
		return service.Backend.ListMaintenanceHolds(ctx)
	case "hold-show":
		id, err := identity.ParseHostID(r.HostID)
		if err != nil {
			return nil, invalid("host ID", err)
		}
		value, exists, err := service.Backend.GetMaintenanceHold(ctx, id)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, notFound("Maintenance Hold")
		}
		return value, nil
	case "incidents-active":
		return service.Backend.ListActiveIncidents(ctx)
	case "incidents-list":
		query := farmmodel.IncidentQuery{State: farmmodel.IncidentState(r.IncidentState), Limit: r.Limit}
		if r.HostID != "" {
			id, err := identity.ParseHostID(r.HostID)
			if err != nil {
				return nil, invalid("host ID", err)
			}
			query.HostID = &id
		}
		return service.Backend.ListIncidents(ctx, query)
	case "status":
		if r.WorkloadID != "" {
			id, err := identity.ParseWorkloadID(r.WorkloadID)
			if err != nil {
				return nil, invalid("workload ID", err)
			}
			return service.workloadStatus(ctx, id)
		}
		return service.Observed.List(), nil
	default:
		return nil, invalid("operator action", nil)
	}
}

func (service *Service) workloadStatus(ctx context.Context, id identity.WorkloadID) (any, error) {
	workload, err := service.Backend.GetDesiredWorkload(ctx, id)
	if err != nil {
		return nil, err
	}
	snapshot, snapshotErr := service.Backend.GetCurrentResolvedSnapshot(ctx, id)
	observed, _ := service.Observed.Get(workload.HostID)
	return struct {
		Workload      farmmodel.DesiredWorkload            `json:"workload"`
		Snapshot      *farmmodel.ResolvedExecutionSnapshot `json:"snapshot,omitempty"`
		Observed      controllerstate.HostObservation      `json:"observed"`
		SnapshotError string                               `json:"snapshot_error,omitempty"`
	}{Workload: workload, Snapshot: func() *farmmodel.ResolvedExecutionSnapshot {
		if snapshotErr == nil {
			return &snapshot
		}
		return nil
	}(), Observed: observed, SnapshotError: func() string {
		if snapshotErr != nil {
			return snapshotErr.Error()
		}
		return ""
	}()}, nil
}

func resourceClaim(r Request) (farmmodel.ResourceClaim, error) {
	claim := farmmodel.ResourceClaim{CPU: r.CPU}
	for _, raw := range r.DeviceIDs {
		id, err := identity.ParseDeviceID(raw)
		if err != nil {
			return claim, invalid("device ID", err)
		}
		claim.DeviceIDs = append(claim.DeviceIDs, id)
	}
	sort.Slice(claim.DeviceIDs, func(i, j int) bool { return claim.DeviceIDs[i].String() < claim.DeviceIDs[j].String() })
	return claim, nil
}
func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func invalid(name string, err error) error {
	message := "invalid " + name
	if err != nil {
		message += ": " + err.Error()
	}
	return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: message}
}
func notFound(name string) error {
	return farmerr.Error{Code: farmerr.NOT_FOUND, HumanMessage: name + " was not found"}
}

func Serve(ctx context.Context, listener net.Listener, service *Service) error {
	var workers sync.WaitGroup
	var connectionsMu sync.Mutex
	connections := make(map[net.Conn]struct{})
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		connectionsMu.Lock()
		for connection := range connections {
			_ = connection.Close()
		}
		connectionsMu.Unlock()
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				workers.Wait()
				return nil
			}
			return err
		}
		connectionsMu.Lock()
		connections[connection] = struct{}{}
		workers.Add(1)
		connectionsMu.Unlock()
		go func() {
			defer workers.Done()
			defer connection.Close()
			serveConnection(connection, service)
			connectionsMu.Lock()
			delete(connections, connection)
			connectionsMu.Unlock()
		}()
	}
}

func serveConnection(connection net.Conn, service *Service) {
	data, err := readFrame(connection, maxRequestBytes)
	if err != nil {
		_ = writeResponse(connection, Response{ErrorCode: string(farmerr.CONFIG_CONFLICT), Error: err.Error()})
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		_ = writeResponse(connection, Response{ErrorCode: string(farmerr.CONFIG_CONFLICT), Error: "invalid operator request"})
		return
	}
	if err := requireJSONEOF(decoder); err != nil {
		_ = writeResponse(connection, Response{ErrorCode: string(farmerr.CONFIG_CONFLICT), Error: "invalid trailing operator request content"})
		return
	}
	var response Response
	if err := service.Execute(request, &response); err != nil {
		response = Response{ErrorCode: string(farmerr.INTERNAL_ERROR), Error: err.Error()}
	}
	_ = writeResponse(connection, response)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func writeResponse(connection io.Writer, response Response) error {
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes+maxRequestBytes {
		return errors.New("operator response frame exceeds limit")
	}
	return writeFrame(connection, data)
}

func readFrame(reader io.Reader, maximum int) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || uint64(size) > uint64(maximum) {
		return nil, errors.New("operator request exceeds limit")
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, err
	}
	return data, nil
}

func writeFrame(writer io.Writer, data []byte) error {
	if uint64(len(data)) > uint64(^uint32(0)) {
		return errors.New("operator frame is too large")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if err := writeAll(writer, header[:]); err != nil {
		return err
	}
	return writeAll(writer, data)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func Call(ctx context.Context, network, address string, request Request) ([]byte, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(data) > maxRequestBytes {
		return nil, errors.New("operator request exceeds limit")
	}
	if err = writeFrame(connection, data); err != nil {
		return nil, err
	}
	data, err = readFrame(connection, maxResponseBytes+maxRequestBytes)
	if err != nil {
		return nil, err
	}
	var response Response
	if err = json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, farmerr.Error{Code: farmerr.Code(response.ErrorCode), HumanMessage: response.Error}
	}
	return response.JSON, nil
}

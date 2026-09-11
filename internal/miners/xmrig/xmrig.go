// Package xmrig implements the XMRig MinerAdapter. XMRig-specific config and
// HTTP API details do not escape this package.
package xmrig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/minerruntime"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/packages"
)

const (
	AdapterID       = "xmrig"
	packageFamilyID = "package_5f5d8ae2b63ce1001aa0a3b8a2a9b29a"
	Version         = "6.26.0"
	ArchiveSHA256   = "fc6f8ae5f64e4f17481f7e3be29a1c56949f216a998414188003eae1db20c9e5"
	UpstreamFee     = 1.0
	maxResponseBody = 1 << 20
)

// PackageID is the stable opaque catalog identity of the XMRig package family.
// All future XMRig versions use this same ID and remain separate by Version.
var PackageID = mustPackageID(packageFamilyID)

func Manifest() packages.Manifest {
	return packages.Manifest{
		PackageID: PackageID, Name: "xmrig", Version: Version, OS: "linux", Architecture: "amd64",
		ArchiveSHA256: ArchiveSHA256, ExecutableRelativePath: "xmrig-6.26.0/xmrig",
		SourceRepository: "https://github.com/xmrig/xmrig",
		SourceURL:        "https://github.com/xmrig/xmrig/releases/download/v6.26.0/xmrig-6.26.0-linux-static-x64.tar.gz",
		VersionArgs:      []string{"--version"}, ExpectedVersion: Version, UpstreamFeePercent: UpstreamFee,
	}
}

type Adapter struct {
	HTTPTimeout time.Duration
	ReadFile    func(string) ([]byte, error)
}

func (a *Adapter) ID() string { return AdapterID }

func (a *Adapter) ProcessSignatures() []model.ProcessSignature {
	return []model.ProcessSignature{{Executable: "xmrig", Provider: AdapterID, CPURelevant: true}}
}

func (a *Adapter) Capabilities() minerruntime.Capabilities {
	return minerruntime.Capabilities{CPU: true, HTTPAPI: true, Benchmark: true, Stress: true, Algorithms: []string{"rx/0", "rx/wow"}}
}

func (a *Adapter) Validate(spec model.MinerSpec, inventory model.Inventory) ([]string, error) {
	if spec.AdapterID != AdapterID || spec.SpecVersion != 1 || spec.PackageID != PackageID || spec.PackageVersion != Version {
		return nil, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid XMRig package or adapter specification"}
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: "XMRig M3 package requires Linux amd64"}
	}
	if inventory.CPU.Threads == 0 {
		return nil, farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: "no usable CPU threads were detected for XMRig"}
	}
	if spec.Mode != model.MinerModeMining && spec.Mode != model.MinerModeStress && spec.Mode != model.MinerModeBenchmark {
		return nil, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "unsupported XMRig mode"}
	}
	if spec.Mode == model.MinerModeMining && (spec.Endpoint == nil || spec.Endpoint.Address == "" || spec.Endpoint.User == "") {
		return nil, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "MINING mode requires a pool endpoint and public login identity"}
	}
	if spec.CPUThreads != nil {
		if *spec.CPUThreads == 0 || (inventory.CPU.Threads > 0 && *spec.CPUThreads > inventory.CPU.Threads) {
			return nil, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "CPU thread count is outside detected limits"}
		}
	}
	if len(spec.GPUDeviceIDs) != 0 {
		return nil, farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: "M3 XMRig adapter supports CPU execution only"}
	}
	for key := range spec.Options {
		return nil, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "unsupported XMRig adapter option", Details: map[string]string{"option": key}}
	}
	warnings := []string{}
	if spec.MSR != nil && *spec.MSR && !msrAvailable() {
		warnings = append(warnings, "MSR requested but unprivileged MSR access is unavailable; MSR writes remain disabled")
	}
	if spec.HugePages != nil && *spec.HugePages && !a.hugePagesAvailable() {
		warnings = append(warnings, "huge pages requested but host support was not detected")
	}
	return warnings, nil
}

func (a *Adapter) Prepare(ctx context.Context, request minerruntime.PrepareRequest) (minerruntime.Prepared, error) {
	if request.Plan.Miner == nil || request.Packages == nil {
		return minerruntime.Prepared{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "XMRig preparation requires MinerSpec and package store"}
	}
	installed, err := request.Packages.Lookup(request.Plan.Miner.PackageID, request.Plan.Miner.PackageVersion)
	if err != nil {
		return minerruntime.Prepared{}, err
	}
	if !sameManifest(installed.Manifest, Manifest()) {
		return minerruntime.Prepared{}, farmerr.Error{Code: farmerr.PACKAGE_HASH_MISMATCH, HumanMessage: "installed XMRig package provenance does not match the pinned manifest"}
	}
	port, err := allocatePort(request.Plan.ExecutionID)
	if err != nil {
		return minerruntime.Prepared{}, farmerr.Error{Code: farmerr.PORT_CONFLICT, HumanMessage: "cannot allocate loopback XMRig API port", Details: map[string]string{"reason": err.Error()}}
	}
	runtimeDir := filepath.Join(request.AgentDataDir, "runtime", request.Plan.ExecutionID.String())
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		return minerruntime.Prepared{}, farmerr.Error{Code: farmerr.PERMISSION_DENIED, HumanMessage: "cannot create XMRig runtime directory"}
	}
	if err := os.Chmod(runtimeDir, 0700); err != nil {
		return minerruntime.Prepared{}, err
	}
	configPath := filepath.Join(runtimeDir, "xmrig-config.json")
	config := buildConfig(*request.Plan.Miner, port)
	if err := atomicConfig(configPath, config); err != nil {
		if _, typed := farmerr.CodeOf(err); typed {
			return minerruntime.Prepared{}, err
		}
		return minerruntime.Prepared{}, farmerr.Error{Code: farmerr.PERMISSION_DENIED, HumanMessage: "cannot persist XMRig runtime config", Details: map[string]string{"reason": err.Error()}}
	}
	resolved := request.Plan
	resolved.Executable = installed.ExecutablePath
	resolved.WorkingDirectory = runtimeDir
	resolved.Environment = nil
	resolved.Args = []string{"--config", configPath}
	switch request.Plan.Miner.Mode {
	case model.MinerModeStress:
		resolved.Args = append(resolved.Args, "--stress")
	case model.MinerModeBenchmark:
		resolved.Args = append(resolved.Args, "--bench=1M")
	}
	if request.Plan.Miner.Algorithm != "" {
		resolved.Args = append(resolved.Args, "--algo", request.Plan.Miner.Algorithm)
	}
	if request.Plan.Miner.CPUThreads != nil {
		resolved.Args = append(resolved.Args, "--threads", strconv.FormatUint(uint64(*request.Plan.Miner.CPUThreads), 10))
	}
	timeout := a.HTTPTimeout
	if timeout <= 0 {
		timeout = time.Second
	}
	source := &Source{URL: fmt.Sprintf("http://127.0.0.1:%d/2/summary", port), Client: localHTTPClient(timeout), HugePagesAvailable: a.hugePagesAvailable(), MSRAvailable: msrAvailable}
	return minerruntime.Prepared{Plan: resolved, Telemetry: source, Cleanup: func() error { return os.RemoveAll(runtimeDir) }}, nil
}

func buildConfig(spec model.MinerSpec, port int) map[string]any {
	hugePages := true
	if spec.HugePages != nil {
		hugePages = *spec.HugePages
	}
	config := map[string]any{
		"autosave": false, "background": false, "colors": false, "title": false, "donate-level": 1,
		"http":    map[string]any{"enabled": true, "host": "127.0.0.1", "port": port, "access-token": nil, "restricted": true},
		"cpu":     map[string]any{"enabled": true, "huge-pages": hugePages, "yield": true},
		"randomx": map[string]any{"1gb-pages": false, "rdmsr": false, "wrmsr": false},
		"opencl":  false, "cuda": false, "log-file": nil,
	}
	if spec.Endpoint != nil && spec.Endpoint.Worker != "" {
		config["api"] = map[string]any{"worker-id": spec.Endpoint.Worker}
	}
	if spec.Mode == model.MinerModeMining {
		pool := map[string]any{"url": spec.Endpoint.Address, "user": spec.Endpoint.User, "pass": spec.Endpoint.Password, "tls": spec.Endpoint.TLS, "keepalive": true}
		if spec.Endpoint.Worker != "" {
			pool["rig-id"] = spec.Endpoint.Worker
		}
		if spec.Coin != "" {
			pool["coin"] = spec.Coin
		}
		config["pools"] = []map[string]any{pool}
	}
	return config
}

func atomicConfig(path string, config map[string]any) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".xmrig-config-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Link(name, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "XMRig runtime config already exists"}
		}
		return err
	}
	if err := os.Remove(name); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func sameManifest(got, want packages.Manifest) bool {
	return got.PackageID == want.PackageID && got.Name == want.Name && got.Version == want.Version &&
		got.OS == want.OS && got.Architecture == want.Architecture && strings.EqualFold(got.ArchiveSHA256, want.ArchiveSHA256) &&
		got.ExecutableRelativePath == want.ExecutableRelativePath && got.SourceRepository == want.SourceRepository &&
		got.SourceURL == want.SourceURL && strings.Join(got.VersionArgs, "\x00") == strings.Join(want.VersionArgs, "\x00") &&
		got.ExpectedVersion == want.ExpectedVersion && got.UpstreamFeePercent == want.UpstreamFeePercent
}

func allocatePort(executionID identity.ExecutionID) (int, error) {
	digest := sha256.Sum256([]byte(executionID.String()))
	const first, count, attempts = 20000, 30000, 128
	start := int(binary.BigEndian.Uint16(digest[:2])) % count
	for offset := 0; offset < attempts; offset++ {
		port := first + (start+offset)%count
		listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		if err := listener.Close(); err != nil {
			return 0, err
		}
		return port, nil
	}
	return 0, errors.New("no loopback API port available in allocation window")
}

type Source struct {
	URL                string
	Client             *http.Client
	HugePagesAvailable bool
	MSRAvailable       func() bool
	Now                func() time.Time
	mu                 sync.Mutex
	lastAccepted       uint64
	haveAccepted       bool
	lastUsefulWorkAt   time.Time
}

func (s *Source) Poll(ctx context.Context) (*model.MinerTelemetry, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, farmerr.Error{Code: farmerr.RPC_UNREACHABLE, HumanMessage: "XMRig HTTP API is unreachable", Details: map[string]string{"reason": err.Error()}}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, farmerr.Error{Code: farmerr.RPC_UNREACHABLE, HumanMessage: "XMRig HTTP API returned an unexpected status"}
	}
	limited := io.LimitReader(response.Body, maxResponseBody+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, farmerr.Error{Code: farmerr.RPC_UNREACHABLE, HumanMessage: "cannot read XMRig API response", Details: map[string]string{"reason": err.Error()}}
	}
	if len(data) > maxResponseBody {
		return nil, farmerr.Error{Code: farmerr.RPC_UNREACHABLE, HumanMessage: "XMRig API response exceeds limit"}
	}
	telemetry, err := ParseSummary(data)
	if err != nil {
		return nil, farmerr.Error{Code: farmerr.RPC_UNREACHABLE, HumanMessage: "XMRig API response is malformed", Details: map[string]string{"reason": err.Error()}}
	}
	telemetry.AdapterID = AdapterID
	hp, msr := s.HugePagesAvailable, false
	if s.MSRAvailable != nil {
		msr = s.MSRAvailable()
	}
	telemetry.HugePagesAvailable, telemetry.MSRAvailable = &hp, &msr
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	telemetry.CollectedAt = now
	telemetry.UsefulWork.CollectedAt = now
	s.mu.Lock()
	if telemetry.AcceptedShares != nil {
		accepted := *telemetry.AcceptedShares
		if accepted > 0 && (!s.haveAccepted || accepted > s.lastAccepted) {
			s.lastUsefulWorkAt = now
		}
		s.lastAccepted, s.haveAccepted = accepted, true
	}
	if !s.lastUsefulWorkAt.IsZero() {
		last := s.lastUsefulWorkAt
		telemetry.UsefulWork.LastUsefulWorkAt = &last
	}
	s.mu.Unlock()
	return telemetry, nil
}

type summary struct {
	Version  string `json:"version"`
	Kind     string `json:"kind"`
	Algo     string `json:"algo"`
	Hashrate struct {
		Total   []*float64 `json:"total"`
		Highest *float64   `json:"highest"`
	} `json:"hashrate"`
	Results struct {
		SharesGood  *uint64 `json:"shares_good"`
		SharesTotal *uint64 `json:"shares_total"`
	} `json:"results"`
	Connection *struct {
		Pool   string  `json:"pool"`
		Uptime uint64  `json:"uptime"`
		Ping   *uint32 `json:"ping"`
	} `json:"connection"`
	Uptime    uint64          `json:"uptime"`
	HugePages json.RawMessage `json:"hugepages"`
}

func ParseSummary(data []byte) (*model.MinerTelemetry, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var native summary
	if err := decoder.Decode(&native); err != nil {
		return nil, err
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	if len(native.Version) > 64 || len(native.Kind) > 64 || len(native.Algo) > 64 {
		return nil, errors.New("XMRig summary identity field exceeds limit")
	}
	for _, value := range native.Hashrate.Total {
		if value != nil && *value < 0 {
			return nil, errors.New("XMRig summary contains a negative hashrate")
		}
	}
	if native.Hashrate.Highest != nil && *native.Hashrate.Highest < 0 {
		return nil, errors.New("XMRig summary contains a negative highest hashrate")
	}
	if native.Results.SharesTotal != nil && native.Results.SharesGood != nil && *native.Results.SharesTotal < *native.Results.SharesGood {
		return nil, errors.New("XMRig summary share counters are inconsistent")
	}
	healthy := true
	telemetry := &model.MinerTelemetry{AdapterID: AdapterID, MinerVersion: native.Version, Algorithm: native.Algo, HighestHashrateHPS: native.Hashrate.Highest, AcceptedShares: native.Results.SharesGood, TotalResults: native.Results.SharesTotal, UptimeSeconds: native.Uptime,
		UsefulWork: &model.UsefulWorkEvidence{Provider: AdapterID, Availability: model.TelemetryAvailable, RuntimeHealthy: &healthy, UsefulWork: model.UsefulWorkUnknown, Upstream: model.UpstreamUnknown, Confidence: model.EvidenceConfidenceAdapterReported}}
	if native.Connection != nil {
		connected := native.Connection.Pool != ""
		telemetry.PoolConnected = &connected
		telemetry.UsefulWork.EndpointVisible = &connected
		if connected {
			telemetry.UsefulWork.Upstream = model.UpstreamConnected
		} else {
			telemetry.UsefulWork.Upstream = model.UpstreamDisconnected
		}
		telemetry.PoolLatencyMS = native.Connection.Ping
		if telemetry.UptimeSeconds == 0 {
			telemetry.UptimeSeconds = native.Connection.Uptime
		}
	}
	if len(native.Hashrate.Total) > 0 {
		telemetry.HashrateShortHPS = native.Hashrate.Total[0]
		if native.Hashrate.Total[0] != nil {
			telemetry.UsefulWork.Metrics = append(telemetry.UsefulWork.Metrics, model.WorkMetric{Kind: "HASHRATE_CURRENT", Unit: "H/S", Value: *native.Hashrate.Total[0]})
		}
	}
	if len(native.Hashrate.Total) > 1 {
		telemetry.HashrateMediumHPS = native.Hashrate.Total[1]
	}
	if len(native.Hashrate.Total) > 2 {
		telemetry.HashrateLongHPS = native.Hashrate.Total[2]
	}
	if native.Results.SharesTotal != nil && native.Results.SharesGood != nil && *native.Results.SharesTotal >= *native.Results.SharesGood {
		rejected := *native.Results.SharesTotal - *native.Results.SharesGood
		telemetry.RejectedShares = &rejected
	}
	telemetry.UsefulWork.AcceptedWork = telemetry.AcceptedShares
	telemetry.UsefulWork.RejectedWork = telemetry.RejectedShares
	telemetry.UsefulWork.StaleWork = telemetry.StaleShares
	connected := telemetry.PoolConnected != nil && *telemetry.PoolConnected
	positive := telemetry.HashrateShortHPS != nil && *telemetry.HashrateShortHPS > 0
	if positive {
		job := true
		telemetry.UsefulWork.JobPresent = &job
	}
	if excessiveRejects(telemetry.AcceptedShares, telemetry.RejectedShares) {
		telemetry.UsefulWork.UsefulWork = model.UsefulWorkNotConfirmed
		telemetry.UsefulWork.ReasonCode = farmerr.TOO_MANY_REJECTS
	} else if positive && connected {
		telemetry.UsefulWork.UsefulWork = model.UsefulWorkConfirmed
	} else if telemetry.HashrateShortHPS != nil {
		telemetry.UsefulWork.UsefulWork = model.UsefulWorkNotConfirmed
		if *telemetry.HashrateShortHPS <= 0 {
			telemetry.UsefulWork.ReasonCode = farmerr.ZERO_HASHRATE
		} else {
			telemetry.UsefulWork.ReasonCode = farmerr.POOL_UNREACHABLE
		}
	} else if telemetry.UsefulWork.Upstream == model.UpstreamDisconnected {
		telemetry.UsefulWork.UsefulWork = model.UsefulWorkNotConfirmed
		telemetry.UsefulWork.ReasonCode = farmerr.POOL_UNREACHABLE
	}
	telemetry.HugePagesPercent = parseHugePages(native.HugePages)
	return telemetry, nil
}

func excessiveRejects(accepted, rejected *uint64) bool {
	if accepted == nil || rejected == nil {
		return false
	}
	total := *accepted + *rejected
	return total >= 10 && float64(*rejected)/float64(total) > 0.20
}

func parseHugePages(data json.RawMessage) *float64 {
	var pair []float64
	if json.Unmarshal(data, &pair) == nil && len(pair) >= 2 && pair[1] > 0 {
		value := pair[0] * 100 / pair[1]
		return &value
	}
	var enabled bool
	if json.Unmarshal(data, &enabled) == nil {
		value := float64(0)
		if enabled {
			value = 100
		}
		return &value
	}
	return nil
}

func localHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: timeout}).DialContext}}
}

func (a *Adapter) hugePagesAvailable() bool {
	read := a.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read("/proc/meminfo")
	return err == nil && strings.Contains(string(data), "Hugepagesize:")
}

func msrAvailable() bool {
	f, err := os.OpenFile("/dev/cpu/0/msr", os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	return f.Close() == nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func mustPackageID(value string) identity.PackageID {
	id, err := identity.ParsePackageID(value)
	if err != nil {
		panic(err)
	}
	return id
}

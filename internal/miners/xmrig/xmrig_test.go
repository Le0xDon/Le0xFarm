package xmrig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/minerruntime"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/packages"
)

func errorCode(err error) farmerr.Code { code, _ := farmerr.CodeOf(err); return code }

func validSpec(t *testing.T, mode model.MinerMode) model.MinerSpec {
	t.Helper()
	threads := uint32(2)
	return model.MinerSpec{AdapterID: AdapterID, SpecVersion: 1, PackageID: PackageID, PackageVersion: Version, Mode: mode, Algorithm: "rx/0", CPUThreads: &threads}
}

func TestValidateModesAndThreads(t *testing.T) {
	adapter := &Adapter{ReadFile: func(string) ([]byte, error) { return []byte("Hugepagesize: 2048 kB"), nil }}
	inventory := model.Inventory{CPU: model.CPU{Threads: 4}}
	if _, err := adapter.Validate(validSpec(t, model.MinerModeStress), inventory); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Validate(validSpec(t, model.MinerModeStress), model.Inventory{}); errorCode(err) != farmerr.INCOMPATIBLE_HARDWARE {
		t.Fatalf("missing CPU error=%v", err)
	}
	mining := validSpec(t, model.MinerModeMining)
	if _, err := adapter.Validate(mining, inventory); errorCode(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("missing endpoint error=%v", err)
	}
	mining.Endpoint = &model.MiningEndpoint{Address: "pool.example:443"}
	if _, err := adapter.Validate(mining, inventory); errorCode(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("missing public login error=%v", err)
	}
	mining.Endpoint = &model.MiningEndpoint{Address: "pool.example:443", TLS: true, User: "public-payout-address", Password: "private-pool-password", Worker: "worker-1"}
	if _, err := adapter.Validate(mining, inventory); err != nil {
		t.Fatal(err)
	}
	tooMany := uint32(5)
	mining.CPUThreads = &tooMany
	if _, err := adapter.Validate(mining, inventory); errorCode(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("threads error=%v", err)
	}
}

func TestMiningConfigUsesResolvedEndpointWithoutInventingLoginSyntax(t *testing.T) {
	spec := validSpec(t, model.MinerModeMining)
	spec.Coin = "ZEPH"
	spec.Endpoint = &model.MiningEndpoint{Address: "xmr.kryptex.network:7029", TLS: false, User: "public-account.worker-from-controller", Password: "x", Worker: "separate-worker"}
	config := buildConfig(spec, 23456)
	pools, ok := config["pools"].([]map[string]any)
	if !ok || len(pools) != 1 {
		t.Fatalf("pools=%#v", config["pools"])
	}
	pool := pools[0]
	if pool["url"] != spec.Endpoint.Address || pool["tls"] != false || pool["user"] != spec.Endpoint.User || pool["pass"] != spec.Endpoint.Password || pool["rig-id"] != spec.Endpoint.Worker || pool["coin"] != "ZEPH" {
		t.Fatalf("resolved endpoint changed: %#v", pool)
	}
	if pool["user"] == "public-account.worker-from-controller.separate-worker" {
		t.Fatal("adapter implicitly concatenated worker to exact pool login")
	}
	spec.Endpoint.TLS = true
	tlsPool := buildConfig(spec, 23457)["pools"].([]map[string]any)[0]
	if tlsPool["tls"] != true {
		t.Fatalf("TLS intent was not preserved: %#v", tlsPool)
	}
}

func TestValidationErrorsDoNotLeakPoolPassword(t *testing.T) {
	spec := validSpec(t, model.MinerModeMining)
	spec.Endpoint = &model.MiningEndpoint{Password: "do-not-leak-this"}
	_, err := (&Adapter{}).Validate(spec, model.Inventory{CPU: model.CPU{Threads: 4}})
	if err == nil || strings.Contains(err.Error(), spec.Endpoint.Password) {
		t.Fatalf("unsafe validation error: %v", err)
	}
}

type fixtureResolver struct {
	installed packages.Installed
	err       error
}

func (r fixtureResolver) Lookup(identity.PackageID, string) (packages.Installed, error) {
	return r.installed, r.err
}

func TestPrepareStressUsesVerifiedPackageAndSafeConfig(t *testing.T) {
	dataDir := t.TempDir()
	planID, _ := identity.NewExecutionID()
	spec := validSpec(t, model.MinerModeStress)
	plan := model.ExecutionPlan{ExecutionID: planID, RestartPolicy: model.RestartOnFailure, Miner: &spec}
	adapter := &Adapter{ReadFile: func(string) ([]byte, error) { return []byte("Hugepagesize: 2048 kB"), nil }}
	prepared, err := adapter.Prepare(context.Background(), minerruntime.PrepareRequest{Plan: plan, AgentDataDir: dataDir, Packages: fixtureResolver{installed: packages.Installed{Manifest: Manifest(), ExecutablePath: "/bin/sleep"}}})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Plan.Executable != "/bin/sleep" || len(prepared.Plan.Args) < 3 || prepared.Plan.Args[0] != "--config" || prepared.Plan.Args[2] != "--stress" {
		t.Fatalf("resolved plan=%+v", prepared.Plan)
	}
	for _, arg := range prepared.Plan.Args {
		if arg == "sh" || arg == "bash" || arg == "-c" {
			t.Fatalf("shell argument present: %q", arg)
		}
	}
	configPath := prepared.Plan.Args[1]
	info, err := os.Stat(configPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions=%v err=%v", info.Mode().Perm(), err)
	}
	data, _ := os.ReadFile(configPath)
	text := string(data)
	if !strings.Contains(text, `"host": "127.0.0.1"`) || strings.Contains(text, "0.0.0.0") || strings.Contains(strings.ToLower(text), "private") || strings.Contains(strings.ToLower(text), "mnemonic") {
		t.Fatalf("unsafe config: %s", text)
	}
	if _, err := adapter.Prepare(context.Background(), minerruntime.PrepareRequest{Plan: plan, AgentDataDir: dataDir, Packages: fixtureResolver{installed: packages.Installed{Manifest: Manifest(), ExecutablePath: "/bin/sleep"}}}); errorCode(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("config overwrite error=%v", err)
	}
	if err := prepared.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(configPath)); !os.IsNotExist(err) {
		t.Fatalf("runtime resources remain: %v", err)
	}
}

func TestPrepareRejectsUnpinnedProvenance(t *testing.T) {
	planID, _ := identity.NewExecutionID()
	spec := validSpec(t, model.MinerModeStress)
	wrong := Manifest()
	wrong.ArchiveSHA256 = strings.Repeat("0", 64)
	_, err := (&Adapter{}).Prepare(context.Background(), minerruntime.PrepareRequest{Plan: model.ExecutionPlan{ExecutionID: planID, Miner: &spec}, AgentDataDir: t.TempDir(), Packages: fixtureResolver{installed: packages.Installed{Manifest: wrong, ExecutablePath: "/bin/sleep"}}})
	if errorCode(err) != farmerr.PACKAGE_HASH_MISMATCH {
		t.Fatalf("provenance error=%v", err)
	}
}

func TestParseSummaryFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "summary-v6.26.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSummary(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.MinerVersion != Version || got.Algorithm != "rx/0" || got.HashrateShortHPS == nil || *got.HashrateShortHPS != 1035.86 || got.HashrateMediumHPS == nil || got.HashrateLongHPS != nil || got.HighestHashrateHPS == nil {
		t.Fatalf("hashrate parse=%+v", got)
	}
	if got.AcceptedShares == nil || *got.AcceptedShares != 0 || got.RejectedShares == nil || *got.RejectedShares != 0 || got.TotalResults == nil || *got.TotalResults != 0 || got.PoolLatencyMS == nil || *got.PoolLatencyMS != 0 || got.HugePagesPercent == nil || *got.HugePagesPercent != 0 {
		t.Fatalf("result parse=%+v", got)
	}
	if got.UsefulWork == nil || got.UsefulWork.Provider != AdapterID || got.UsefulWork.Availability != model.TelemetryAvailable || got.UsefulWork.UsefulWork != model.UsefulWorkConfirmed || got.UsefulWork.Upstream != model.UpstreamConnected || got.UsefulWork.AcceptedWork == nil || len(got.UsefulWork.Metrics) != 1 || got.UsefulWork.Metrics[0].Kind != "HASHRATE_CURRENT" {
		t.Fatalf("useful-work evidence=%+v", got.UsefulWork)
	}
	if _, err := ParseSummary([]byte("{")); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	if got, err := ParseSummary([]byte(`{"version":"6.26.0","future":true}`)); err != nil || got.MinerVersion != Version {
		t.Fatalf("missing/unknown fields: %+v %v", got, err)
	}
	if _, err := ParseSummary([]byte(`{"hashrate":{"total":[-1]}}`)); err == nil {
		t.Fatal("negative hashrate accepted")
	}
	if _, err := ParseSummary([]byte(`{"results":{"shares_good":2,"shares_total":1}}`)); err == nil {
		t.Fatal("inconsistent share counters accepted")
	}
}

func TestParseSummaryDistinguishesZeroUnknownUnavailableAndProviderHealth(t *testing.T) {
	zero, err := ParseSummary([]byte(`{"version":"6.26.0","hashrate":{"total":[0]},"results":{"shares_good":0,"shares_total":0},"connection":{"pool":"pool.example:443"}}`))
	if err != nil || zero.HashrateShortHPS == nil || *zero.HashrateShortHPS != 0 || zero.UsefulWork.UsefulWork != model.UsefulWorkNotConfirmed || zero.UsefulWork.ReasonCode != farmerr.ZERO_HASHRATE {
		t.Fatalf("explicit zero=%+v err=%v", zero, err)
	}
	unknown, err := ParseSummary([]byte(`{"version":"6.26.0","hashrate":{"total":[]},"results":{},"connection":{"pool":"pool.example:443"}}`))
	if err != nil || unknown.HashrateShortHPS != nil || unknown.UsefulWork.UsefulWork != model.UsefulWorkUnknown || unknown.UsefulWork.ReasonCode != "" {
		t.Fatalf("unknown hashrate=%+v err=%v", unknown, err)
	}
	disconnected, err := ParseSummary([]byte(`{"version":"6.26.0","hashrate":{"total":[10]},"results":{},"connection":{"pool":""}}`))
	if err != nil || disconnected.UsefulWork.Upstream != model.UpstreamDisconnected || disconnected.UsefulWork.ReasonCode != farmerr.POOL_UNREACHABLE {
		t.Fatalf("disconnected=%+v err=%v", disconnected, err)
	}
	rejects, err := ParseSummary([]byte(`{"version":"6.26.0","hashrate":{"total":[10]},"results":{"shares_good":7,"shares_total":10},"connection":{"pool":"pool.example:443"}}`))
	if err != nil || rejects.UsefulWork.ReasonCode != farmerr.TOO_MANY_REJECTS || rejects.UsefulWork.UsefulWork != model.UsefulWorkNotConfirmed {
		t.Fatalf("reject evidence=%+v err=%v", rejects, err)
	}
}

func TestSourceTracksAcceptedWorkWithoutLeakingEndpoint(t *testing.T) {
	now := time.Unix(10_000, 0).UTC()
	payload := []byte(`{"version":"6.26.0","hashrate":{"total":[10]},"results":{"shares_good":1,"shares_total":1},"connection":{"pool":"secret-user:secret-password@private.pool.example:443"}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) }))
	defer server.Close()
	source := &Source{URL: server.URL, Client: localHTTPClient(time.Second), MSRAvailable: func() bool { return false }, Now: func() time.Time { return now }}
	got, err := source.Poll(context.Background())
	if err != nil || got.UsefulWork.LastUsefulWorkAt == nil || !got.UsefulWork.LastUsefulWorkAt.Equal(now) || !got.UsefulWork.CollectedAt.Equal(now) {
		t.Fatalf("accepted work timestamp=%+v err=%v", got, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "secret-user") || strings.Contains(string(encoded), "secret-password") || strings.Contains(string(encoded), "private.pool.example") {
		t.Fatalf("endpoint leaked in normalized telemetry: %s err=%v", encoded, err)
	}
}

func TestSourceLimitsTimeoutAndRecovery(t *testing.T) {
	fixture, _ := os.ReadFile(filepath.Join("testdata", "summary-v6.26.0.json"))
	var behavior atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch behavior.Load() {
		case 0:
			time.Sleep(30 * time.Millisecond)
		case 1:
			_, _ = w.Write(make([]byte, maxResponseBody+1))
		default:
			_, _ = w.Write(fixture)
		}
	}))
	defer server.Close()
	source := &Source{URL: server.URL, Client: localHTTPClient(5 * time.Millisecond), MSRAvailable: func() bool { return false }}
	if _, err := source.Poll(context.Background()); errorCode(err) != farmerr.RPC_UNREACHABLE {
		t.Fatalf("timeout error=%v", err)
	}
	behavior.Store(1)
	source.Client = localHTTPClient(time.Second)
	if _, err := source.Poll(context.Background()); errorCode(err) != farmerr.RPC_UNREACHABLE {
		t.Fatalf("size error=%v", err)
	}
	behavior.Store(2)
	got, err := source.Poll(context.Background())
	if err != nil || got.HashrateShortHPS == nil {
		t.Fatalf("recovery=%+v err=%v", got, err)
	}
}

func TestManifestIsPinnedAndTransparent(t *testing.T) {
	m := Manifest()
	if m.ArchiveSHA256 != ArchiveSHA256 || m.Version != Version || m.SourceRepository != "https://github.com/xmrig/xmrig" || m.UpstreamFeePercent != 1 {
		t.Fatalf("manifest=%+v", m)
	}
	if _, err := identity.ParsePackageID(m.PackageID.String()); err != nil {
		t.Fatal(err)
	}
}

func TestPackageIDIdentifiesFamilyIndependentlyOfVersion(t *testing.T) {
	current := Manifest()
	futureRepresentation := current
	futureRepresentation.Version = "6.27.0"
	futureRepresentation.ExecutableRelativePath = "xmrig-6.27.0/xmrig"
	if current.PackageID != futureRepresentation.PackageID {
		t.Fatal("changing version changed the XMRig package-family identity")
	}
	if current.Version == futureRepresentation.Version {
		t.Fatal("PackageID and Version are not distinct identity dimensions")
	}
	if current.PackageID.String() != packageFamilyID || strings.Contains(current.PackageID.String(), "6260") || strings.Contains(current.PackageID.String(), "xmrig") {
		t.Fatalf("PackageID is not the fixed opaque family identity: %s", current.PackageID)
	}
}

func TestMinerSpecHasNoPrivateWalletMaterial(t *testing.T) {
	typeOf := reflect.TypeOf(model.MinerSpec{})
	for i := 0; i < typeOf.NumField(); i++ {
		name := strings.ToLower(typeOf.Field(i).Name)
		if strings.Contains(name, "seed") || strings.Contains(name, "mnemonic") || strings.Contains(name, "private") {
			t.Fatalf("private wallet material field exists: %s", typeOf.Field(i).Name)
		}
	}
}

func TestAPIPortAllocationIsLoopbackAndDeterministic(t *testing.T) {
	id, _ := identity.NewExecutionID()
	first, err := allocatePort(id)
	if err != nil {
		t.Fatal(err)
	}
	second, err := allocatePort(id)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first < 20000 || first >= 50000 {
		t.Fatalf("ports=%d,%d", first, second)
	}
}

func TestAtomicConfigNeverOverwritesConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	configs := []map[string]any{{"marker": "first"}, {"marker": "second"}}
	var wg sync.WaitGroup
	errs := make(chan error, len(configs))
	for _, config := range configs {
		wg.Add(1)
		go func(value map[string]any) {
			defer wg.Done()
			errs <- atomicConfig(path, value)
		}(config)
	}
	wg.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else if errorCode(err) == farmerr.CONFIG_CONFLICT {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	data, err := os.ReadFile(path)
	if err != nil || (!strings.Contains(string(data), "first") && !strings.Contains(string(data), "second")) {
		t.Fatalf("published config=%q err=%v", data, err)
	}
}

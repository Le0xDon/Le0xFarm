package farmresolve

import (
	"context"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/packagecatalog"
)

func TestLoginResolutionPolicies(t *testing.T) {
	tests := []struct {
		name, template               string
		placement                    farmmodel.WorkerPlacement
		worker, wantUser, wantWorker string
	}{
		{"wallet", "${wallet}", farmmodel.WorkerNone, "", "public-wallet", ""},
		{"in-user", "${wallet}.${worker}", farmmodel.WorkerInUser, "rig-1", "public-wallet.rig-1", ""},
		{"separate", "${wallet}", farmmodel.WorkerSeparate, "rig-1", "public-wallet", "rig-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputs, resolver := fixture(t)
			inputs.Profile.LoginPolicy = farmmodel.LoginPolicy{UserTemplate: test.template, WorkerPlacement: test.placement}
			inputs.Workload.Worker = test.worker
			result, err := resolver.Resolve(context.Background(), inputs)
			if err != nil {
				t.Fatal(err)
			}
			if result.Content.Endpoint.User != test.wantUser || result.Content.Endpoint.Worker != test.wantWorker {
				t.Fatalf("endpoint=%+v", result.Content.Endpoint)
			}
			if !result.Content.Endpoint.TLS || result.Content.Endpoint.Password != "x" {
				t.Fatalf("TLS/public literal lost: %+v", result.Content.Endpoint)
			}
		})
	}
}

func TestWorkerRequirementFailsClosed(t *testing.T) {
	inputs, resolver := fixture(t)
	inputs.Profile.LoginPolicy = farmmodel.LoginPolicy{UserTemplate: "${wallet}.${worker}", WorkerPlacement: farmmodel.WorkerInUser}
	inputs.Workload.Worker = ""
	_, err := resolver.Resolve(context.Background(), inputs)
	assertCode(t, err, farmerr.CONFIG_CONFLICT)
}

func TestResolvedHashExcludesNamesAndRevisions(t *testing.T) {
	inputs, resolver := fixture(t)
	first, err := resolver.Resolve(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	inputs.Pool.Name = "renamed"
	inputs.Pool.Meta.Revision++
	inputs.Wallet.Name = "renamed"
	inputs.Wallet.Meta.Revision++
	inputs.Profile.Name = "renamed"
	inputs.Profile.Meta.Revision++
	inputs.Workload.Name = "renamed"
	inputs.Workload.Meta.Revision++
	second, err := resolver.Resolve(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash {
		t.Fatal("display metadata changed resolved hash")
	}
	inputs.Pool.Address = "other.example:444"
	changed, _ := resolver.Resolve(context.Background(), inputs)
	if changed.Hash == first.Hash {
		t.Fatal("pool address did not change resolved hash")
	}
}

func TestResolvedHashRuntimeDimensions(t *testing.T) {
	base, resolver := fixture(t)
	first, _ := resolver.Resolve(context.Background(), base)
	tests := map[string]func(*Inputs){
		"tls": func(i *Inputs) { i.Pool.TLS = false }, "wallet": func(i *Inputs) { i.Wallet.Address = "other-public" }, "worker": func(i *Inputs) { i.Workload.Worker = "rig-2" },
		"version": func(i *Inputs) { i.Profile.Package.Version = "2.0" }, "threads": func(i *Inputs) { v := uint32(4); i.Profile.CPUThreads = &v },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			copy := base
			mutate(&copy)
			local := resolver
			if name == "version" {
				local.Catalog = catalog(t, copy.Profile.Package)
			}
			got, err := local.Resolve(context.Background(), copy)
			if err != nil {
				t.Fatal(err)
			}
			if got.Hash == first.Hash {
				t.Fatal("runtime change did not change hash")
			}
		})
	}
}

func fixture(t *testing.T) (Inputs, Resolver) {
	t.Helper()
	packageID := mustPackage(t, "package_11111111111111111111111111111111")
	poolID := mustPool(t, "pool_11111111111111111111111111111111")
	walletID := mustWallet(t, "wallet_11111111111111111111111111111111")
	profileID := mustProfile(t, "profile_11111111111111111111111111111111")
	hostID := mustHost(t, "host_11111111111111111111111111111111")
	workloadID := mustWorkload(t, "workload_11111111111111111111111111111111")
	threads := uint32(2)
	meta := farmmodel.ObjectMeta{SchemaVersion: 1, Revision: 1, ContentHash: "unused", Origin: farmmodel.OriginController, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	inputs := Inputs{Workload: farmmodel.DesiredWorkload{WorkloadID: workloadID, Meta: meta, DesiredGeneration: 1, DesiredWorkloadContent: farmmodel.DesiredWorkloadContent{Name: "work", HostID: hostID, ProfileID: profileID, RunState: farmmodel.DesiredRunning, Worker: "rig-1", Resources: farmmodel.ResourceClaim{CPU: true}}}, Profile: farmmodel.MiningProfile{ProfileID: profileID, Meta: meta, MiningProfileContent: farmmodel.MiningProfileContent{Name: "profile", AdapterID: "miner", Package: farmmodel.PackageRef{PackageID: packageID, Version: "1.0"}, Mode: farmmodel.ProfileModeMining, Coin: "COIN", PoolID: poolID, WalletID: walletID, LoginPolicy: farmmodel.LoginPolicy{UserTemplate: "${wallet}.${worker}", WorkerPlacement: farmmodel.WorkerInUser}, CPUThreads: &threads}}, Pool: farmmodel.Pool{PoolID: poolID, Meta: meta, PoolContent: farmmodel.PoolContent{Name: "pool", Address: "pool.example:443", TLS: true, Auth: farmmodel.PoolAuth{Kind: farmmodel.PoolAuthPublicLiteral, PublicLiteral: "x"}}}, Wallet: farmmodel.WalletRef{WalletID: walletID, Meta: meta, WalletRefContent: farmmodel.WalletRefContent{Name: "wallet", Coin: "COIN", Address: "public-wallet"}}}
	return inputs, Resolver{Catalog: catalog(t, inputs.Profile.Package)}
}

func catalog(t *testing.T, ref farmmodel.PackageRef) farmmodel.PackageCatalog {
	t.Helper()
	value, err := packagecatalog.NewStatic([]farmmodel.PackageRelease{{Ref: ref, AdapterIDs: []string{"miner"}}})
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func assertCode(t *testing.T, err error, want farmerr.Code) {
	t.Helper()
	code, ok := farmerr.CodeOf(err)
	if !ok || code != want {
		t.Fatalf("code=%s want=%s err=%v", code, want, err)
	}
}
func mustPackage(t *testing.T, s string) identity.PackageID {
	id, e := identity.ParsePackageID(s)
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func mustPool(t *testing.T, s string) identity.PoolID {
	id, e := identity.ParsePoolID(s)
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func mustWallet(t *testing.T, s string) identity.WalletID {
	id, e := identity.ParseWalletID(s)
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func mustProfile(t *testing.T, s string) identity.ProfileID {
	id, e := identity.ParseProfileID(s)
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func mustHost(t *testing.T, s string) identity.HostID {
	id, e := identity.ParseHostID(s)
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func mustWorkload(t *testing.T, s string) identity.WorkloadID {
	id, e := identity.ParseWorkloadID(s)
	if e != nil {
		t.Fatal(e)
	}
	return id
}

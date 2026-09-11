package farmconfig

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerdb"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/packagecatalog"
)

var testPackageID = mustPackageID("package_11111111111111111111111111111111")

func TestCRUDReferencesRevisionAndPersistence(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "controller")
	db, service := newService(t, dir, Options{})
	defer db.Close()
	pool, err := service.CreatePool(ctx, poolContent("Primary"))
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := service.CreateWalletRef(ctx, walletContent("Payout"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := service.CreateMiningProfile(ctx, profileContent(pool.PoolID, wallet.WalletID, "Profile"))
	if err != nil {
		t.Fatal(err)
	}
	if pool.Meta.Origin != farmmodel.OriginController || wallet.Meta.Origin != farmmodel.OriginController || profile.Meta.Origin != farmmodel.OriginController {
		t.Fatalf("normal CRUD origin must be CONTROLLER: pool=%s wallet=%s profile=%s", pool.Meta.Origin, wallet.Meta.Origin, profile.Meta.Origin)
	}
	if pool.Meta.Revision != 1 || wallet.Meta.Revision != 1 || profile.Meta.Revision != 1 {
		t.Fatal("created revision must be one")
	}
	if len(mustListPools(t, service)) != 1 {
		t.Fatal("Pool list missing object")
	}
	profiles, err := service.ListMiningProfiles(ctx)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles: %v %v", profiles, err)
	}

	updatedContent := pool.PoolContent
	updatedContent.Name = "Renamed"
	updated, err := service.UpdatePool(ctx, pool.PoolID, 1, updatedContent)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Meta.Revision != 2 || updated.Meta.ContentHash == pool.Meta.ContentHash {
		t.Fatal("Pool update did not change revision/hash")
	}
	if updated.Meta.Origin != farmmodel.OriginController {
		t.Fatalf("update changed origin to %s", updated.Meta.Origin)
	}
	_, err = service.UpdatePool(ctx, pool.PoolID, 1, updatedContent)
	assertCode(t, err, farmerr.REVISION_CONFLICT)
	unchanged, err := service.UpdatePool(ctx, pool.PoolID, 2, updatedContent)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Meta.Revision != 2 {
		t.Fatal("identical update changed revision")
	}

	assertCode(t, service.DeletePool(ctx, pool.PoolID, 2), farmerr.REFERENCE_IN_USE)
	assertCode(t, service.DeleteWalletRef(ctx, wallet.WalletID, 1), farmerr.REFERENCE_IN_USE)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, service = newService(t, dir, Options{})
	defer db.Close()
	reloaded, err := service.GetMiningProfile(ctx, profile.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PoolID != pool.PoolID || reloaded.WalletID != wallet.WalletID {
		t.Fatal("references did not persist")
	}
	if err := service.DeleteMiningProfile(ctx, profile.ProfileID, 1); err != nil {
		t.Fatal(err)
	}
	if err := service.DeletePool(ctx, pool.PoolID, 2); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteWalletRef(ctx, wallet.WalletID, 1); err != nil {
		t.Fatal(err)
	}
	_, err = service.GetPool(ctx, pool.PoolID)
	assertCode(t, err, farmerr.NOT_FOUND)
}

func TestProfileRejectsInvalidReferencesAndCatalog(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	ctx := context.Background()
	pool, _ := service.CreatePool(ctx, poolContent("P"))
	wallet, _ := service.CreateWalletRef(ctx, walletContent("W"))
	missingPool, _ := identity.ParsePoolID("pool_22222222222222222222222222222222")
	_, err := service.CreateMiningProfile(ctx, profileContent(missingPool, wallet.WalletID, "bad"))
	assertCode(t, err, farmerr.INVALID_REFERENCE)
	badPackage := profileContent(pool.PoolID, wallet.WalletID, "bad package")
	badPackage.Package.Version = "9.9"
	_, err = service.CreateMiningProfile(ctx, badPackage)
	assertCode(t, err, farmerr.INVALID_REFERENCE)
	badAdapter := profileContent(pool.PoolID, wallet.WalletID, "bad adapter")
	badAdapter.AdapterID = "other"
	_, err = service.CreateMiningProfile(ctx, badAdapter)
	assertCode(t, err, farmerr.INVALID_REFERENCE)
}

func TestConcurrentUpdateAllowsOneExpectedRevisionWinner(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	ctx := context.Background()
	pool, err := service.CreatePool(ctx, poolContent("base"))
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 12
	start := make(chan struct{})
	codes := make(chan farmerr.Code, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			content := pool.PoolContent
			content.Name = fmt.Sprintf("edit-%d", i)
			_, err := service.UpdatePool(ctx, pool.PoolID, 1, content)
			if err == nil {
				codes <- "SUCCESS"
				return
			}
			code, _ := farmerr.CodeOf(err)
			codes <- code
		}(i)
	}
	close(start)
	wg.Wait()
	close(codes)
	success, conflicts := 0, 0
	for code := range codes {
		switch code {
		case "SUCCESS":
			success++
		case farmerr.REVISION_CONFLICT:
			conflicts++
		default:
			t.Errorf("unexpected code %s", code)
		}
	}
	if success != 1 || conflicts != contenders-1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
}

func TestGeneratedIDCollisionReturnsAlreadyExists(t *testing.T) {
	fixed, _ := identity.ParsePoolID("pool_33333333333333333333333333333333")
	db, service := newService(t, t.TempDir(), Options{NewPoolID: func() (identity.PoolID, error) { return fixed, nil }})
	defer db.Close()
	if _, err := service.CreatePool(context.Background(), poolContent("one")); err != nil {
		t.Fatal(err)
	}
	_, err := service.CreatePool(context.Background(), poolContent("two"))
	assertCode(t, err, farmerr.ALREADY_EXISTS)
}

func TestPublicLiteralRoundTripAsExplicitPlaintext(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	content := poolContent("literal")
	content.Auth = farmmodel.PoolAuth{Kind: farmmodel.PoolAuthPublicLiteral, PublicLiteral: "x"}
	created, err := service.CreatePool(context.Background(), content)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := service.GetPool(context.Background(), created.PoolID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Auth != content.Auth {
		t.Fatalf("auth changed: %+v", loaded.Auth)
	}
}

func TestWalletAndProfileUpdateLifecycle(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	ctx := context.Background()
	pool, _ := service.CreatePool(ctx, poolContent("pool"))
	wallet, _ := service.CreateWalletRef(ctx, walletContent("wallet"))
	secondWallet, _ := service.CreateWalletRef(ctx, walletContent("wallet-2"))
	profile, _ := service.CreateMiningProfile(ctx, profileContent(pool.PoolID, wallet.WalletID, "profile"))

	walletUpdate := wallet.WalletRefContent
	walletUpdate.Address = "new-public-payout-reference"
	updatedWallet, err := service.UpdateWalletRef(ctx, wallet.WalletID, 1, walletUpdate)
	if err != nil || updatedWallet.Meta.Revision != 2 {
		t.Fatalf("WalletRef update: %+v %v", updatedWallet, err)
	}
	_, err = service.UpdateWalletRef(ctx, wallet.WalletID, 1, walletUpdate)
	assertCode(t, err, farmerr.REVISION_CONFLICT)

	profileUpdate := profile.MiningProfileContent
	profileUpdate.WalletID = secondWallet.WalletID
	profileUpdate.Algorithm = "new-algorithm"
	updatedProfile, err := service.UpdateMiningProfile(ctx, profile.ProfileID, 1, profileUpdate)
	if err != nil || updatedProfile.Meta.Revision != 2 || updatedProfile.WalletID != secondWallet.WalletID {
		t.Fatalf("MiningProfile update: %+v %v", updatedProfile, err)
	}
	loaded, err := service.GetWalletRef(ctx, wallet.WalletID)
	if err != nil || loaded.Address != walletUpdate.Address {
		t.Fatalf("WalletRef get: %+v %v", loaded, err)
	}
	items, err := service.ListWalletRefs(ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("WalletRef list: %+v %v", items, err)
	}
	if err := service.DeleteWalletRef(ctx, wallet.WalletID, 2); err != nil {
		t.Fatal("old reference remained after profile update:", err)
	}
	assertCode(t, service.DeleteMiningProfile(ctx, profile.ProfileID, 1), farmerr.REVISION_CONFLICT)
}

func TestNewRequiresPersistentDependencies(t *testing.T) {
	db, err := controllerdb.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = New(db, nil, Options{})
	assertCode(t, err, farmerr.CONFIG_CONFLICT)
}

func TestCorruptStoredObjectFailsClosed(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	created, err := service.CreatePool(context.Background(), poolContent("pool"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec("UPDATE pools SET name='tampered' WHERE pool_id=?", created.PoolID.String()); err != nil {
		t.Fatal(err)
	}
	_, err = service.GetPool(context.Background(), created.PoolID)
	assertCode(t, err, farmerr.CONFIG_CONFLICT)
}

func newService(t *testing.T, dir string, options Options) (*controllerdb.DB, *Service) {
	t.Helper()
	db, err := controllerdb.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := packagecatalog.NewStatic([]farmmodel.PackageRelease{
		{Ref: farmmodel.PackageRef{PackageID: testPackageID, Version: "1.0"}, AdapterIDs: []string{"generic-miner"}, Runtime: farmmodel.RuntimeCapabilities{CPU: true, GPU: true, MultiGPUSingleProcess: true}, Tuning: farmmodel.TuningCapabilities{CPUThreads: true, HugePages: true, MSR: true}},
		{Ref: farmmodel.PackageRef{PackageID: testPackageID, Version: "2.0"}, AdapterIDs: []string{"generic-miner"}, Runtime: farmmodel.RuntimeCapabilities{CPU: true, GPU: true, MultiGPUSingleProcess: true}, Tuning: farmmodel.TuningCapabilities{CPUThreads: true, HugePages: true, MSR: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Clock == nil {
		fixed := time.Date(2026, 9, 8, 12, 0, 0, 123, time.UTC)
		options.Clock = func() time.Time { return fixed }
	}
	service, err := New(db, catalog, options)
	if err != nil {
		t.Fatal(err)
	}
	return db, service
}

func poolContent(name string) farmmodel.PoolContent {
	return farmmodel.PoolContent{Name: name, Address: "pool.example:443", TLS: true, Auth: farmmodel.PoolAuth{Kind: farmmodel.PoolAuthNone}}
}
func walletContent(name string) farmmodel.WalletRefContent {
	return farmmodel.WalletRefContent{Name: name, Coin: "COIN", Address: "public-payout-reference"}
}
func profileContent(pool identity.PoolID, wallet identity.WalletID, name string) farmmodel.MiningProfileContent {
	threads := uint32(2)
	huge := true
	msr := false
	return farmmodel.MiningProfileContent{Name: name, AdapterID: "generic-miner", Package: farmmodel.PackageRef{PackageID: testPackageID, Version: "1.0"}, Mode: farmmodel.ProfileModeMining, Coin: "COIN", Algorithm: "algo", PoolID: pool, WalletID: wallet, LoginPolicy: farmmodel.LoginPolicy{UserTemplate: "${wallet}.${worker}", WorkerPlacement: farmmodel.WorkerInUser}, CPUThreads: &threads, HugePages: &huge, MSR: &msr}
}
func mustListPools(t *testing.T, service *Service) []farmmodel.Pool {
	t.Helper()
	items, err := service.ListPools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return items
}
func assertCode(t *testing.T, err error, want farmerr.Code) {
	t.Helper()
	code, ok := farmerr.CodeOf(err)
	if !ok || code != want {
		t.Fatalf("code=%s want=%s err=%v", code, want, err)
	}
}
func mustPackageID(value string) identity.PackageID {
	id, err := identity.ParsePackageID(value)
	if err != nil {
		panic(err)
	}
	return id
}

package farmmodel

import (
	"bytes"
	"strings"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

func TestPoolHasNoCoinAndPublicLiteralIsExplicitPlaintext(t *testing.T) {
	content := PoolContent{Name: "Pool", Address: "pool.example:443", TLS: true, Auth: PoolAuth{Kind: PoolAuthPublicLiteral, PublicLiteral: "x"}}
	if err := ValidatePool(content); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(PublicLiteralNotice, "plaintext") || !strings.Contains(PublicLiteralNotice, "must not contain") {
		t.Fatalf("notice is not explicit: %q", PublicLiteralNotice)
	}
}

func TestLoginPolicyValidation(t *testing.T) {
	valid := []LoginPolicy{
		{UserTemplate: "${wallet}", WorkerPlacement: WorkerNone},
		{UserTemplate: "${wallet}.${worker}", WorkerPlacement: WorkerInUser},
		{UserTemplate: "${wallet}", WorkerPlacement: WorkerSeparate},
	}
	for _, policy := range valid {
		if err := ValidateLoginPolicy(policy); err != nil {
			t.Errorf("valid policy rejected: %v", err)
		}
	}
	invalid := []LoginPolicy{
		{UserTemplate: "literal", WorkerPlacement: WorkerNone},
		{UserTemplate: "${wallet}.${unknown}", WorkerPlacement: WorkerNone},
		{UserTemplate: "${wallet}.${worker}", WorkerPlacement: WorkerSeparate},
		{UserTemplate: "${wallet}", WorkerPlacement: WorkerInUser},
	}
	for _, policy := range invalid {
		if code := codeOf(t, ValidateLoginPolicy(policy)); code != farmerr.CONFIG_CONFLICT {
			t.Errorf("got %s", code)
		}
	}
}

func TestContentHashDeterministicAndLogical(t *testing.T) {
	content := PoolContent{Name: "A", Address: "pool.example:1", TLS: false, Auth: PoolAuth{Kind: PoolAuthNone}}
	first, err := PoolHash(content)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PoolHash(content)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "sha256:") || len(first) != 71 {
		t.Fatalf("unexpected deterministic hash: %q %q", first, second)
	}
	content.Name = "B"
	changed, _ := PoolHash(content)
	if changed == first {
		t.Fatal("logical content change did not change hash")
	}
}

func TestMiningProfileHashDistinguishesPackageFamilyAndVersion(t *testing.T) {
	packageID := mustPackage(t, "package_11111111111111111111111111111111")
	poolID, _ := identity.ParsePoolID("pool_11111111111111111111111111111111")
	walletID, _ := identity.ParseWalletID("wallet_11111111111111111111111111111111")
	content := MiningProfileContent{Name: "P", AdapterID: "adapter", Package: PackageRef{PackageID: packageID, Version: "1.0"}, Mode: ProfileModeMining, Coin: "COIN", PoolID: poolID, WalletID: walletID, LoginPolicy: LoginPolicy{UserTemplate: "${wallet}", WorkerPlacement: WorkerNone}}
	first, _ := MiningProfileHash(content)
	content.Package.Version = "2.0"
	second, _ := MiningProfileHash(content)
	if first == second || content.Package.PackageID != packageID {
		t.Fatal("PackageID and Version are not distinct hash dimensions")
	}
}

func TestCanonicalJSONUsesRFC8785StringEscaping(t *testing.T) {
	var output bytes.Buffer
	if err := appendCanonical(&output, "<public>&value>\n"); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != `"<public>&value>\n"` {
		t.Fatalf("canonical string=%s", got)
	}
}

func TestResourceClaimValidationAndNormalization(t *testing.T) {
	first, _ := identity.ParseDeviceID("device_22222222222222222222222222222222")
	second, _ := identity.ParseDeviceID("device_11111111111111111111111111111111")
	claim := ResourceClaim{DeviceIDs: []identity.DeviceID{first, second}}
	if err := ValidateResourceClaim(claim); err != nil {
		t.Fatal(err)
	}
	normalized := NormalizeResourceClaim(claim)
	if normalized.DeviceIDs[0] != second || claim.DeviceIDs[0] != first {
		t.Fatal("ResourceClaim was not deterministically copied and sorted")
	}
	if code := codeOf(t, ValidateResourceClaim(ResourceClaim{})); code != farmerr.CONFIG_CONFLICT {
		t.Fatalf("empty claim code %s", code)
	}
	if code := codeOf(t, ValidateResourceClaim(ResourceClaim{DeviceIDs: []identity.DeviceID{first, first}})); code != farmerr.CONFIG_CONFLICT {
		t.Fatalf("duplicate claim code %s", code)
	}
}

func mustPackage(t *testing.T, value string) identity.PackageID {
	t.Helper()
	id, err := identity.ParsePackageID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func codeOf(t *testing.T, err error) farmerr.Code {
	t.Helper()
	code, ok := farmerr.CodeOf(err)
	if !ok {
		t.Fatalf("not typed: %v", err)
	}
	return code
}

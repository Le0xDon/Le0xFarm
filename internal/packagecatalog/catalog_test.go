package packagecatalog

import (
	"context"
	"slices"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/miners/xmrig"
)

func TestStaticCatalogLookup(t *testing.T) {
	id, _ := identity.ParsePackageID("package_11111111111111111111111111111111")
	ref := farmmodel.PackageRef{PackageID: id, Version: "1.0"}
	catalog, err := NewStatic([]farmmodel.PackageRelease{{Ref: ref, AdapterIDs: []string{"miner"}}})
	if err != nil {
		t.Fatal(err)
	}
	release, err := catalog.Lookup(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if release.Ref != ref || len(release.AdapterIDs) != 1 {
		t.Fatalf("unexpected release: %+v", release)
	}
	_, err = catalog.Lookup(context.Background(), farmmodel.PackageRef{PackageID: id, Version: "2.0"})
	if code, _ := farmerr.CodeOf(err); code != farmerr.NOT_FOUND {
		t.Fatalf("got %v", err)
	}
}

func TestStaticCatalogRejectsDuplicate(t *testing.T) {
	id, _ := identity.ParsePackageID("package_11111111111111111111111111111111")
	release := farmmodel.PackageRelease{Ref: farmmodel.PackageRef{PackageID: id, Version: "1"}, AdapterIDs: []string{"a"}}
	_, err := NewStatic([]farmmodel.PackageRelease{release, release})
	if code, _ := farmerr.CodeOf(err); code != farmerr.ALREADY_EXISTS {
		t.Fatalf("got %v", err)
	}
}

func TestBuiltinCatalogContainsPinnedXMRig(t *testing.T) {
	catalog, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	manifest := xmrig.Manifest()
	release, err := catalog.Lookup(context.Background(), farmmodel.PackageRef{PackageID: manifest.PackageID, Version: manifest.Version})
	if err != nil || !slices.Contains(release.AdapterIDs, xmrig.AdapterID) {
		t.Fatalf("release=%+v err=%v", release, err)
	}
}

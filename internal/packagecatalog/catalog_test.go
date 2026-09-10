package packagecatalog

import (
	"context"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
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

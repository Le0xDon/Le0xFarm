// Package packagecatalog provides Controller-side, read-only package release metadata.
package packagecatalog

import (
	"context"
	"sync"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
)

type Static struct {
	mu       sync.RWMutex
	releases map[string]farmmodel.PackageRelease
}

func NewStatic(releases []farmmodel.PackageRelease) (*Static, error) {
	catalog := &Static{releases: make(map[string]farmmodel.PackageRelease, len(releases))}
	for _, release := range releases {
		if err := farmmodel.ValidatePackageRef(release.Ref); err != nil || len(release.AdapterIDs) == 0 {
			return nil, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid package catalog release"}
		}
		key := catalogKey(release.Ref)
		if _, exists := catalog.releases[key]; exists {
			return nil, farmerr.Error{Code: farmerr.ALREADY_EXISTS, HumanMessage: "duplicate package catalog release"}
		}
		copyRelease := release
		copyRelease.AdapterIDs = append([]string(nil), release.AdapterIDs...)
		catalog.releases[key] = copyRelease
	}
	return catalog, nil
}

func (catalog *Static) Lookup(_ context.Context, ref farmmodel.PackageRef) (farmmodel.PackageRelease, error) {
	if err := farmmodel.ValidatePackageRef(ref); err != nil {
		return farmmodel.PackageRelease{}, err
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	release, ok := catalog.releases[catalogKey(ref)]
	if !ok {
		return farmmodel.PackageRelease{}, farmerr.Error{Code: farmerr.NOT_FOUND, HumanMessage: "package release is not present in the Controller catalog"}
	}
	release.AdapterIDs = append([]string(nil), release.AdapterIDs...)
	return release, nil
}

func catalogKey(ref farmmodel.PackageRef) string {
	return ref.PackageID.String() + "\x00" + ref.Version
}

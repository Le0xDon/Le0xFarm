// Package gpuresource resolves stable GPU claims against current physical
// inventory without making scheduling or policy decisions.
package gpuresource

import (
	"regexp"
	"slices"
	"strings"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

var pciSelectorPattern = regexp.MustCompile(`^(?:[0-9a-f]{4}:)?[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// Resolve creates deterministic bindings for the exact requested DeviceIDs.
// UUID is the first M6 stable-identity source. GPUs without it are observable
// but cannot safely be selected for runtime ownership.
func Resolve(requested []identity.DeviceID, inventory model.Inventory, allowedVendors []string) ([]model.GPUAssignment, error) {
	byID := make(map[identity.DeviceID]model.GPU, len(inventory.GPUs))
	identities := make(map[string]identity.DeviceID, len(inventory.GPUs))
	selectors := make(map[string]identity.DeviceID, len(inventory.GPUs))
	for _, gpu := range inventory.GPUs {
		if err := gpu.DeviceID.Validate(); err != nil {
			return nil, incompatible("fresh inventory contains an invalid GPU DeviceID", nil)
		}
		if _, exists := byID[gpu.DeviceID]; exists {
			return nil, incompatible("fresh inventory contains a duplicate GPU DeviceID", map[string]string{"device_id": gpu.DeviceID.String()})
		}
		byID[gpu.DeviceID] = gpu
		identityValue := NormalizeHardwareText(gpu.UUID)
		selector := strings.ToLower(strings.TrimSpace(gpu.PCIBusID))
		if identityValue != "" {
			if other, exists := identities[identityValue]; exists && other != gpu.DeviceID {
				return nil, incompatible("fresh inventory contains an ambiguous GPU hardware identity", nil)
			}
			identities[identityValue] = gpu.DeviceID
		}
		if selector != "" {
			if other, exists := selectors[selector]; exists && other != gpu.DeviceID {
				return nil, incompatible("fresh inventory contains an ambiguous GPU runtime selector", nil)
			}
			selectors[selector] = gpu.DeviceID
		}
	}

	result := make([]model.GPUAssignment, 0, len(requested))
	seen := make(map[identity.DeviceID]struct{}, len(requested))
	for _, deviceID := range requested {
		if err := deviceID.Validate(); err != nil {
			return nil, incompatible("requested GPU DeviceID is invalid", nil)
		}
		if _, exists := seen[deviceID]; exists {
			return nil, incompatible("requested GPU DeviceID is duplicated", map[string]string{"device_id": deviceID.String()})
		}
		seen[deviceID] = struct{}{}
		gpu, ok := byID[deviceID]
		if !ok {
			return nil, incompatible("requested DeviceID is absent from fresh inventory", map[string]string{"device_id": deviceID.String()})
		}
		hardwareIdentity := NormalizeHardwareText(gpu.UUID)
		if !validHardwareIdentity(hardwareIdentity) {
			return nil, incompatible("requested GPU has no stable hardware identity", map[string]string{"device_id": deviceID.String()})
		}
		selector := strings.ToLower(strings.TrimSpace(gpu.PCIBusID))
		if !pciSelectorPattern.MatchString(selector) {
			return nil, incompatible("requested GPU has no valid runtime selector", map[string]string{"device_id": deviceID.String()})
		}
		vendor := NormalizeHardwareText(gpu.Vendor)
		if len(allowedVendors) != 0 && !containsNormalized(allowedVendors, vendor) {
			return nil, incompatible("requested GPU vendor is incompatible with package/adapter", map[string]string{"device_id": deviceID.String(), "vendor": vendor})
		}
		result = append(result, model.GPUAssignment{DeviceID: deviceID, HardwareIdentity: hardwareIdentity, RuntimeSelector: selector})
	}
	slices.SortFunc(result, func(a, b model.GPUAssignment) int { return strings.Compare(a.DeviceID.String(), b.DeviceID.String()) })
	return result, nil
}

// ValidateCurrent proves that an immutable binding still names exactly the
// same current devices, identities and selectors.
func ValidateCurrent(requested []identity.DeviceID, assignments []model.GPUAssignment, inventory model.Inventory, allowedVendors []string) error {
	if err := ValidateBindings(requested, assignments); err != nil {
		return err
	}
	current, err := Resolve(requested, inventory, allowedVendors)
	if err != nil {
		return err
	}
	if !slices.Equal(current, assignments) {
		return incompatible("resolved GPU binding does not match fresh inventory", nil)
	}
	return nil
}

// ValidateStableIdentities permits a current runtime selector to change (for
// example after moving the same physical GPU to another PCI slot), but rejects
// a different hardware identity claiming an already-bound DeviceID.
func ValidateStableIdentities(requested []identity.DeviceID, assignments []model.GPUAssignment, inventory model.Inventory, allowedVendors []string) error {
	if err := ValidateBindings(requested, assignments); err != nil {
		return err
	}
	current, err := Resolve(requested, inventory, allowedVendors)
	if err != nil {
		return err
	}
	for i := range current {
		if current[i].DeviceID != assignments[i].DeviceID || current[i].HardwareIdentity != assignments[i].HardwareIdentity {
			return incompatible("GPU DeviceID resolves to a different physical hardware identity", map[string]string{"device_id": assignments[i].DeviceID.String()})
		}
	}
	return nil
}

// ValidateBindings validates an already canonical immutable binding without
// consulting mutable inventory.
func ValidateBindings(requested []identity.DeviceID, assignments []model.GPUAssignment) error {
	if len(requested) != len(assignments) {
		return incompatible("GPU assignment count does not match ResourceClaim", nil)
	}
	normalizedIDs := append([]identity.DeviceID(nil), requested...)
	slices.SortFunc(normalizedIDs, func(a, b identity.DeviceID) int { return strings.Compare(a.String(), b.String()) })
	for i, assignment := range assignments {
		if assignment.DeviceID != normalizedIDs[i] || !validHardwareIdentity(assignment.HardwareIdentity) || NormalizeHardwareText(assignment.HardwareIdentity) != assignment.HardwareIdentity ||
			!pciSelectorPattern.MatchString(assignment.RuntimeSelector) || strings.ToLower(strings.TrimSpace(assignment.RuntimeSelector)) != assignment.RuntimeSelector {
			return incompatible("GPU assignment is malformed or not canonical", map[string]string{"device_id": assignment.DeviceID.String()})
		}
	}
	return nil
}

func NormalizeHardwareText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func validHardwareIdentity(value string) bool {
	switch value {
	case "", "unknown", "none", "n/a", "not available":
		return false
	}
	return strings.Trim(value, "0-:._ ") != ""
}

func containsNormalized(values []string, wanted string) bool {
	for _, value := range values {
		if NormalizeHardwareText(value) == wanted {
			return true
		}
	}
	return false
}

func incompatible(message string, details map[string]string) error {
	return farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: message, Details: details}
}

// Package identity defines opaque, comparable identifiers generated independently of hostnames.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

func generate(prefix string) (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate %s ID: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(entropy[:]), nil
}

func validate(value, prefix string) error {
	if len(value) != len(prefix)+1+32 || !strings.HasPrefix(value, prefix+"_") {
		return fmt.Errorf("invalid %s ID format", prefix)
	}
	for _, c := range value[len(prefix)+1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return fmt.Errorf("invalid %s ID encoding", prefix)
		}
	}
	return nil
}

// FarmID is an opaque value identifier. Its zero value is invalid.
type FarmID struct{ value string }

func NewFarmID() (FarmID, error) {
	value, err := generate("farm")
	return FarmID{value: value}, err
}

func ParseFarmID(value string) (FarmID, error) {
	if err := validate(value, "farm"); err != nil {
		return FarmID{}, err
	}
	return FarmID{value: value}, nil
}

func (id FarmID) String() string  { return id.value }
func (id FarmID) Validate() error { return validate(id.value, "farm") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id FarmID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// ControllerID is an opaque value identifier. Its zero value is invalid.
type ControllerID struct{ value string }

func NewControllerID() (ControllerID, error) {
	value, err := generate("controller")
	return ControllerID{value: value}, err
}

func ParseControllerID(value string) (ControllerID, error) {
	if err := validate(value, "controller"); err != nil {
		return ControllerID{}, err
	}
	return ControllerID{value: value}, nil
}

func (id ControllerID) String() string  { return id.value }
func (id ControllerID) Validate() error { return validate(id.value, "controller") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id ControllerID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// HostID is an opaque value identifier. Its zero value is invalid.
type HostID struct{ value string }

func NewHostID() (HostID, error) {
	value, err := generate("host")
	return HostID{value: value}, err
}

func ParseHostID(value string) (HostID, error) {
	if err := validate(value, "host"); err != nil {
		return HostID{}, err
	}
	return HostID{value: value}, nil
}

func (id HostID) String() string  { return id.value }
func (id HostID) Validate() error { return validate(id.value, "host") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id HostID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// AgentID is an opaque value identifier. Its zero value is invalid.
type AgentID struct{ value string }

func NewAgentID() (AgentID, error) {
	value, err := generate("agent")
	return AgentID{value: value}, err
}

func ParseAgentID(value string) (AgentID, error) {
	if err := validate(value, "agent"); err != nil {
		return AgentID{}, err
	}
	return AgentID{value: value}, nil
}

func (id AgentID) String() string  { return id.value }
func (id AgentID) Validate() error { return validate(id.value, "agent") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id AgentID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// NodaID is an opaque value identifier. Its zero value is invalid.
type NodaID struct{ value string }

func NewNodaID() (NodaID, error) {
	value, err := generate("noda")
	return NodaID{value: value}, err
}

func ParseNodaID(value string) (NodaID, error) {
	if err := validate(value, "noda"); err != nil {
		return NodaID{}, err
	}
	return NodaID{value: value}, nil
}

func (id NodaID) String() string  { return id.value }
func (id NodaID) Validate() error { return validate(id.value, "noda") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id NodaID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// DeviceID is an opaque value identifier. Its zero value is invalid.
type DeviceID struct{ value string }

func NewDeviceID() (DeviceID, error) {
	value, err := generate("device")
	return DeviceID{value: value}, err
}

func ParseDeviceID(value string) (DeviceID, error) {
	if err := validate(value, "device"); err != nil {
		return DeviceID{}, err
	}
	return DeviceID{value: value}, nil
}

func (id DeviceID) String() string  { return id.value }
func (id DeviceID) Validate() error { return validate(id.value, "device") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id DeviceID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// ProfileID is an opaque value identifier. Its zero value is invalid.
type ProfileID struct{ value string }

func NewProfileID() (ProfileID, error) {
	value, err := generate("profile")
	return ProfileID{value: value}, err
}

func ParseProfileID(value string) (ProfileID, error) {
	if err := validate(value, "profile"); err != nil {
		return ProfileID{}, err
	}
	return ProfileID{value: value}, nil
}

func (id ProfileID) String() string  { return id.value }
func (id ProfileID) Validate() error { return validate(id.value, "profile") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id ProfileID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// ExecutionID is an opaque value identifier. Its zero value is invalid.
type ExecutionID struct{ value string }

func NewExecutionID() (ExecutionID, error) {
	value, err := generate("execution")
	return ExecutionID{value: value}, err
}

func ParseExecutionID(value string) (ExecutionID, error) {
	if err := validate(value, "execution"); err != nil {
		return ExecutionID{}, err
	}
	return ExecutionID{value: value}, nil
}

func (id ExecutionID) String() string  { return id.value }
func (id ExecutionID) Validate() error { return validate(id.value, "execution") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id ExecutionID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// ServiceID is an opaque value identifier. Its zero value is invalid.
type ServiceID struct{ value string }

func NewServiceID() (ServiceID, error) {
	value, err := generate("service")
	return ServiceID{value: value}, err
}

func ParseServiceID(value string) (ServiceID, error) {
	if err := validate(value, "service"); err != nil {
		return ServiceID{}, err
	}
	return ServiceID{value: value}, nil
}

func (id ServiceID) String() string  { return id.value }
func (id ServiceID) Validate() error { return validate(id.value, "service") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id ServiceID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// WalletID is an opaque value identifier. Its zero value is invalid.
type WalletID struct{ value string }

func NewWalletID() (WalletID, error) {
	value, err := generate("wallet")
	return WalletID{value: value}, err
}

func ParseWalletID(value string) (WalletID, error) {
	if err := validate(value, "wallet"); err != nil {
		return WalletID{}, err
	}
	return WalletID{value: value}, nil
}

func (id WalletID) String() string  { return id.value }
func (id WalletID) Validate() error { return validate(id.value, "wallet") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id WalletID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// PoolID is an opaque value identifier. Its zero value is invalid.
type PoolID struct{ value string }

func NewPoolID() (PoolID, error) {
	value, err := generate("pool")
	return PoolID{value: value}, err
}

func ParsePoolID(value string) (PoolID, error) {
	if err := validate(value, "pool"); err != nil {
		return PoolID{}, err
	}
	return PoolID{value: value}, nil
}

func (id PoolID) String() string  { return id.value }
func (id PoolID) Validate() error { return validate(id.value, "pool") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id PoolID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// PackageID is an opaque value identifier. Its zero value is invalid.
type PackageID struct{ value string }

func NewPackageID() (PackageID, error) {
	value, err := generate("package")
	return PackageID{value: value}, err
}

func ParsePackageID(value string) (PackageID, error) {
	if err := validate(value, "package"); err != nil {
		return PackageID{}, err
	}
	return PackageID{value: value}, nil
}

func (id PackageID) String() string  { return id.value }
func (id PackageID) Validate() error { return validate(id.value, "package") }

// MarshalText supports JSON string values and map keys without exposing mutable fields.
func (id PackageID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// WorkloadID identifies persistent desired workload intent. It is distinct
// from ExecutionID, which identifies one concrete runtime execution.
type WorkloadID struct{ value string }

func NewWorkloadID() (WorkloadID, error) {
	value, err := generate("workload")
	return WorkloadID{value: value}, err
}

func ParseWorkloadID(value string) (WorkloadID, error) {
	if err := validate(value, "workload"); err != nil {
		return WorkloadID{}, err
	}
	return WorkloadID{value: value}, nil
}

func (id WorkloadID) String() string  { return id.value }
func (id WorkloadID) Validate() error { return validate(id.value, "workload") }

func (id WorkloadID) MarshalText() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id.value), nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *FarmID) UnmarshalText(text []byte) error {
	parsed, err := ParseFarmID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *ControllerID) UnmarshalText(text []byte) error {
	parsed, err := ParseControllerID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *HostID) UnmarshalText(text []byte) error {
	parsed, err := ParseHostID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *AgentID) UnmarshalText(text []byte) error {
	parsed, err := ParseAgentID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *NodaID) UnmarshalText(text []byte) error {
	parsed, err := ParseNodaID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *DeviceID) UnmarshalText(text []byte) error {
	parsed, err := ParseDeviceID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *ProfileID) UnmarshalText(text []byte) error {
	parsed, err := ParseProfileID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *ExecutionID) UnmarshalText(text []byte) error {
	parsed, err := ParseExecutionID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *ServiceID) UnmarshalText(text []byte) error {
	parsed, err := ParseServiceID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *WalletID) UnmarshalText(text []byte) error {
	parsed, err := ParseWalletID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *PoolID) UnmarshalText(text []byte) error {
	parsed, err := ParsePoolID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *PackageID) UnmarshalText(text []byte) error {
	parsed, err := ParsePackageID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// UnmarshalText replaces the ID only after successful validation.
func (id *WorkloadID) UnmarshalText(text []byte) error {
	parsed, err := ParseWorkloadID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// Package farmerr defines structured domain errors.
package farmerr

import "errors"

type Code string

const (
	MISSING_DEPENDENCY           Code = "MISSING_DEPENDENCY"
	MISSING_COMMAND              Code = "MISSING_COMMAND"
	MISSING_WALLET               Code = "MISSING_WALLET"
	MISSING_POOL                 Code = "MISSING_POOL"
	PACKAGE_NOT_INSTALLED        Code = "PACKAGE_NOT_INSTALLED"
	PACKAGE_HASH_MISMATCH        Code = "PACKAGE_HASH_MISMATCH"
	INCOMPATIBLE_HARDWARE        Code = "INCOMPATIBLE_HARDWARE"
	INSUFFICIENT_DISK            Code = "INSUFFICIENT_DISK"
	SERVICE_NOT_READY            Code = "SERVICE_NOT_READY"
	RPC_UNREACHABLE              Code = "RPC_UNREACHABLE"
	POOL_UNREACHABLE             Code = "POOL_UNREACHABLE"
	STARTUP_TIMEOUT              Code = "STARTUP_TIMEOUT"
	ZERO_HASHRATE                Code = "ZERO_HASHRATE"
	TOO_MANY_REJECTS             Code = "TOO_MANY_REJECTS"
	PERMISSION_DENIED            Code = "PERMISSION_DENIED"
	PORT_CONFLICT                Code = "PORT_CONFLICT"
	PROCESS_CRASHED              Code = "PROCESS_CRASHED"
	MAINTENANCE_HOLD             Code = "MAINTENANCE_HOLD"
	UNMANAGED_PROCESS_CONFLICT   Code = "UNMANAGED_PROCESS_CONFLICT"
	UNMANAGED_OBSERVATION_FAILED Code = "UNMANAGED_OBSERVATION_FAILED"
	CONFIG_CONFLICT              Code = "CONFIG_CONFLICT"
	SIGNATURE_INVALID            Code = "SIGNATURE_INVALID"
	PROTOCOL_VERSION_MISMATCH    Code = "PROTOCOL_VERSION_MISMATCH"
	SCHEMA_VERSION_MISMATCH      Code = "SCHEMA_VERSION_MISMATCH"
	PAIRING_REQUIRED             Code = "PAIRING_REQUIRED"
	PAIRING_TOKEN_INVALID        Code = "PAIRING_TOKEN_INVALID"
	PAIRING_TOKEN_EXPIRED        Code = "PAIRING_TOKEN_EXPIRED"
	CONTROLLER_IDENTITY_MISMATCH Code = "CONTROLLER_IDENTITY_MISMATCH"
	TLS_CREDENTIALS_REQUIRED     Code = "TLS_CREDENTIALS_REQUIRED"
	TLS_FINGERPRINT_MISMATCH     Code = "TLS_FINGERPRINT_MISMATCH"
	TLS_IDENTITY_MISMATCH        Code = "TLS_IDENTITY_MISMATCH"
	CERTIFICATE_EXPIRED          Code = "CERTIFICATE_EXPIRED"
	NOT_FOUND                    Code = "NOT_FOUND"
	ALREADY_EXISTS               Code = "ALREADY_EXISTS"
	REVISION_CONFLICT            Code = "REVISION_CONFLICT"
	REFERENCE_IN_USE             Code = "REFERENCE_IN_USE"
	INVALID_REFERENCE            Code = "INVALID_REFERENCE"
	INTERNAL_ERROR               Code = "INTERNAL_ERROR"
)

type Error struct {
	Code         Code              `json:"code"`
	HumanMessage string            `json:"human_message"`
	Details      map[string]string `json:"details,omitempty"`
	SuggestedFix string            `json:"suggested_fix,omitempty"`
	LogsRef      string            `json:"logs_ref,omitempty"`
}

// CodeOf returns a typed domain code without inspecting human-readable text.
func CodeOf(err error) (Code, bool) {
	var typed Error
	if !errors.As(err, &typed) {
		return "", false
	}
	return typed.Code, true
}

func (e Error) Error() string {
	if e.HumanMessage == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.HumanMessage
}

// Package farmmodel defines persistent Controller-side farm configuration.
package farmmodel

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const (
	ObjectSchemaVersion uint32 = 1
	// PublicLiteralNotice is the application-boundary warning for persisted pool auth.
	PublicLiteralNotice = "PUBLIC_LITERAL is stored as plaintext in farm.db and must not contain credentials or secrets"
)

type Origin string

const (
	OriginController Origin = "CONTROLLER"
	OriginBuiltin    Origin = "BUILTIN"
)

type ObjectMeta struct {
	SchemaVersion uint32
	Revision      uint64
	ContentHash   string
	Origin        Origin
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type PoolAuthKind string

const (
	PoolAuthNone          PoolAuthKind = "NONE"
	PoolAuthPublicLiteral PoolAuthKind = "PUBLIC_LITERAL"
)

type PoolAuth struct {
	Kind          PoolAuthKind
	PublicLiteral string
}

type PoolContent struct {
	Name    string
	Address string
	TLS     bool
	Auth    PoolAuth
}

type Pool struct {
	PoolID identity.PoolID
	Meta   ObjectMeta
	PoolContent
}

type WalletRefContent struct {
	Name    string
	Coin    string
	Address string
}

type WalletRef struct {
	WalletID identity.WalletID
	Meta     ObjectMeta
	WalletRefContent
}

type PackageRef struct {
	PackageID identity.PackageID
	Version   string
}

type WorkerPlacement string

const (
	WorkerNone     WorkerPlacement = "NONE"
	WorkerInUser   WorkerPlacement = "IN_USER"
	WorkerSeparate WorkerPlacement = "SEPARATE"
)

// LoginPolicy deterministically resolves a public miner-facing login in M4.2.
// UserTemplate supports only ${wallet} and, for IN_USER, ${worker}.
type LoginPolicy struct {
	UserTemplate    string
	WorkerPlacement WorkerPlacement
}

type ProfileMode string

const ProfileModeMining ProfileMode = "MINING"

type MiningProfileContent struct {
	Name        string
	AdapterID   string
	Package     PackageRef
	Mode        ProfileMode
	Coin        string
	Algorithm   string
	PoolID      identity.PoolID
	WalletID    identity.WalletID
	LoginPolicy LoginPolicy
	CPUThreads  *uint32
	HugePages   *bool
	MSR         *bool
}

type MiningProfile struct {
	ProfileID identity.ProfileID
	Meta      ObjectMeta
	MiningProfileContent
}

// HostProfileSettingsContent contains only explicit Host+Profile tuning
// overrides. Nil means inherit the MiningProfile default.
type HostProfileSettingsContent struct {
	CPUThreads *uint32
	HugePages  *bool
	MSR        *bool
}

// HostProfileSettings is keyed by the immutable HostID+ProfileID pair. It is
// Controller-owned configuration, never observed hardware state.
type HostProfileSettings struct {
	HostID    identity.HostID
	ProfileID identity.ProfileID
	Meta      ObjectMeta
	HostProfileSettingsContent
}

func ValidatePool(content PoolContent) error {
	if err := validateName(content.Name); err != nil {
		return invalid("invalid Pool name", err)
	}
	if len(content.Address) > 512 || strings.ContainsRune(content.Address, 0) {
		return invalid("invalid Pool address", nil)
	}
	host, portText, err := net.SplitHostPort(content.Address)
	if err != nil || strings.TrimSpace(host) == "" {
		return invalid("Pool address must be host:port", err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return invalid("Pool address must contain a valid TCP port", err)
	}
	switch content.Auth.Kind {
	case PoolAuthNone:
		if content.Auth.PublicLiteral != "" {
			return invalid("NONE Pool auth cannot contain a public literal", nil)
		}
	case PoolAuthPublicLiteral:
		if content.Auth.PublicLiteral == "" || len(content.Auth.PublicLiteral) > 256 || strings.ContainsRune(content.Auth.PublicLiteral, 0) {
			return invalid("PUBLIC_LITERAL Pool auth requires a bounded non-empty value", nil)
		}
	default:
		return invalid("unknown Pool auth kind", nil)
	}
	return nil
}

func ValidateWalletRef(content WalletRefContent) error {
	if err := validateName(content.Name); err != nil {
		return invalid("invalid WalletRef name", err)
	}
	if err := validateToken("coin", content.Coin, 64); err != nil {
		return err
	}
	if strings.TrimSpace(content.Address) == "" || len(content.Address) > 1024 || strings.ContainsRune(content.Address, 0) {
		return invalid("WalletRef public address must be non-empty and bounded", nil)
	}
	return nil
}

func ValidatePackageRef(ref PackageRef) error {
	if err := ref.PackageID.Validate(); err != nil {
		return invalid("invalid PackageID", err)
	}
	if err := validateToken("package version", ref.Version, 128); err != nil {
		return err
	}
	if ref.Version == "." || ref.Version == ".." || strings.ContainsAny(ref.Version, `/\\`) {
		return invalid("invalid package version", nil)
	}
	return nil
}

func ValidateLoginPolicy(policy LoginPolicy) error {
	if policy.UserTemplate == "" || len(policy.UserTemplate) > 1024 || strings.ContainsRune(policy.UserTemplate, 0) || !utf8.ValidString(policy.UserTemplate) {
		return invalid("login user template must be non-empty valid UTF-8", nil)
	}
	if strings.Count(policy.UserTemplate, "${wallet}") != 1 {
		return invalid("login user template must contain ${wallet} exactly once", nil)
	}
	remainder := strings.ReplaceAll(strings.ReplaceAll(policy.UserTemplate, "${wallet}", ""), "${worker}", "")
	if strings.Contains(remainder, "${") || strings.ContainsAny(policy.UserTemplate, "\r\n") {
		return invalid("login user template contains an unsupported placeholder or control character", nil)
	}
	workers := strings.Count(policy.UserTemplate, "${worker}")
	switch policy.WorkerPlacement {
	case WorkerNone, WorkerSeparate:
		if workers != 0 {
			return invalid("worker placeholder is not allowed for this worker placement", nil)
		}
	case WorkerInUser:
		if workers != 1 {
			return invalid("IN_USER login template must contain ${worker} exactly once", nil)
		}
	default:
		return invalid("unknown worker placement", nil)
	}
	return nil
}

func ValidateMiningProfile(content MiningProfileContent) error {
	if err := validateName(content.Name); err != nil {
		return invalid("invalid MiningProfile name", err)
	}
	if err := validateToken("adapter ID", content.AdapterID, 128); err != nil {
		return err
	}
	if err := ValidatePackageRef(content.Package); err != nil {
		return err
	}
	if content.Mode != ProfileModeMining {
		return invalid("M4 MiningProfile mode must be MINING", nil)
	}
	if err := validateToken("coin", content.Coin, 64); err != nil {
		return err
	}
	if len(content.Algorithm) > 128 || strings.ContainsRune(content.Algorithm, 0) {
		return invalid("invalid algorithm", nil)
	}
	if err := content.PoolID.Validate(); err != nil {
		return invalid("invalid Pool reference", err)
	}
	if err := content.WalletID.Validate(); err != nil {
		return invalid("invalid WalletRef reference", err)
	}
	if err := ValidateLoginPolicy(content.LoginPolicy); err != nil {
		return err
	}
	if content.CPUThreads != nil && *content.CPUThreads == 0 {
		return invalid("CPU threads must be at least one when set", nil)
	}
	return nil
}

func ValidateHostProfileSettings(hostID identity.HostID, profileID identity.ProfileID, content HostProfileSettingsContent) error {
	if err := hostID.Validate(); err != nil {
		return invalid("invalid HostProfileSettings HostID", err)
	}
	if err := profileID.Validate(); err != nil {
		return invalid("invalid HostProfileSettings ProfileID", err)
	}
	if content.CPUThreads != nil && *content.CPUThreads == 0 {
		return invalid("HostProfileSettings CPU threads must be at least one when set", nil)
	}
	if content.CPUThreads == nil && content.HugePages == nil && content.MSR == nil {
		return invalid("HostProfileSettings must contain at least one explicit override", nil)
	}
	return nil
}

func ValidateTuningCapabilities(cpuThreads *uint32, hugePages, msr *bool, capabilities TuningCapabilities) error {
	if cpuThreads != nil && !capabilities.CPUThreads {
		return invalid("package/adapter does not support CPUThreads", nil)
	}
	if hugePages != nil && !capabilities.HugePages {
		return invalid("package/adapter does not support HugePages", nil)
	}
	if msr != nil && !capabilities.MSR {
		return invalid("package/adapter does not support MSR", nil)
	}
	return nil
}

func validateName(value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 256 || strings.ContainsRune(value, 0) || !utf8.ValidString(value) {
		return fmt.Errorf("name must be non-empty valid UTF-8 and at most 256 bytes")
	}
	return nil
}

func validateToken(label, value string, max int) error {
	if strings.TrimSpace(value) == "" || len(value) > max || strings.ContainsRune(value, 0) || strings.ContainsAny(value, "\r\n") {
		return invalid("invalid "+label, nil)
	}
	return nil
}

func invalid(message string, cause error) error {
	details := map[string]string{}
	if cause != nil {
		details["cause"] = cause.Error()
	}
	return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: message, Details: details}
}

// PackageCatalog is the read-only catalog boundary used by profile validation.
type PackageCatalog interface {
	Lookup(context.Context, PackageRef) (PackageRelease, error)
}

type PackageRelease struct {
	Ref        PackageRef
	AdapterIDs []string
	Tuning     TuningCapabilities
}

// TuningCapabilities are Controller-side declarations for the narrow typed
// settings that may be placed into a resolved plan. They do not grant any
// executable or privileged behavior.
type TuningCapabilities struct {
	CPUThreads bool
	HugePages  bool
	MSR        bool
}

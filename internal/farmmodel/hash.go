package farmmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf8"
)

// ContentHash uses Le0xFarm's constrained deterministic JSON representation.
// Supported values are objects with sorted fixed-schema keys, arrays, valid
// UTF-8 strings, booleans, null and integers. Floating-point values are not
// supported. The logical content maps below deliberately omit metadata and IDs.
func ContentHash(value any) (string, error) {
	var buf bytes.Buffer
	if err := appendCanonical(&buf, value); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func PoolHash(content PoolContent) (string, error) {
	return ContentHash(map[string]any{
		"address": content.Address,
		"auth": map[string]any{
			"kind":           string(content.Auth.Kind),
			"public_literal": content.Auth.PublicLiteral,
		},
		"name": content.Name,
		"tls":  content.TLS,
	})
}

func WalletRefHash(content WalletRefContent) (string, error) {
	return ContentHash(map[string]any{"address": content.Address, "coin": content.Coin, "name": content.Name})
}

func MiningProfileHash(content MiningProfileContent) (string, error) {
	return ContentHash(map[string]any{
		"adapter_id":  content.AdapterID,
		"algorithm":   content.Algorithm,
		"coin":        content.Coin,
		"cpu_threads": optionalUint32(content.CPUThreads),
		"huge_pages":  optionalBool(content.HugePages),
		"login_policy": map[string]any{
			"user_template":    content.LoginPolicy.UserTemplate,
			"worker_placement": string(content.LoginPolicy.WorkerPlacement),
		},
		"mode": string(content.Mode),
		"msr":  optionalBool(content.MSR),
		"name": content.Name,
		"package": map[string]any{
			"package_id": content.Package.PackageID.String(),
			"version":    content.Package.Version,
		},
		"pool_id":   content.PoolID.String(),
		"wallet_id": content.WalletID.String(),
	})
}

func HostProfileSettingsHash(content HostProfileSettingsContent) (string, error) {
	return ContentHash(map[string]any{
		"cpu_threads": optionalUint32(content.CPUThreads),
		"huge_pages":  optionalBool(content.HugePages), "msr": optionalBool(content.MSR),
	})
}

func DesiredWorkloadHash(content DesiredWorkloadContent) (string, error) {
	claim := NormalizeResourceClaim(content.Resources)
	return ContentHash(map[string]any{
		"host_id": content.HostID.String(), "name": content.Name,
		"profile_id": content.ProfileID.String(), "run_state": string(content.RunState),
		"worker": content.Worker, "resources": resourceClaimHashValue(claim),
	})
}

func ResolvedRuntimeHash(content ResolvedRuntimeContent) (string, error) {
	claim := NormalizeResourceClaim(content.Resources)
	return ContentHash(map[string]any{
		"run_state": string(content.RunState), "host_id": content.HostID.String(),
		"profile_id": content.ProfileID.String(),
		"resources":  resourceClaimHashValue(claim), "adapter_id": content.AdapterID,
		"package": map[string]any{"package_id": content.Package.PackageID.String(), "version": content.Package.Version},
		"mode":    string(content.Mode), "coin": content.Coin, "algorithm": content.Algorithm,
		"endpoint":    map[string]any{"address": content.Endpoint.Address, "tls": content.Endpoint.TLS, "user": content.Endpoint.User, "password": content.Endpoint.Password, "worker": content.Endpoint.Worker},
		"cpu_threads": optionalUint32(content.CPUThreads), "huge_pages": optionalBool(content.HugePages), "msr": optionalBool(content.MSR),
	})
}

func resourceClaimHashValue(claim ResourceClaim) map[string]any {
	devices := make([]any, len(claim.DeviceIDs))
	for i, id := range claim.DeviceIDs {
		devices[i] = id.String()
	}
	return map[string]any{"cpu": claim.CPU, "device_ids": devices}
}

func optionalUint32(value *uint32) any {
	if value == nil {
		return nil
	}
	return uint64(*value)
}

func optionalBool(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

func appendCanonical(buf *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buf.WriteString("null")
	case string:
		if err := appendJSONString(buf, typed); err != nil {
			return err
		}
	case bool:
		buf.WriteString(strconv.FormatBool(typed))
	case uint32:
		buf.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(typed, 10))
	case int:
		buf.WriteString(strconv.Itoa(typed))
	case []any:
		buf.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := appendCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := appendJSONString(buf, key); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := appendCanonical(buf, typed[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON value %T", value)
	}
	return nil
}

func appendJSONString(buf *bytes.Buffer, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("canonical JSON string is not valid UTF-8")
	}
	buf.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			buf.WriteByte('\\')
			buf.WriteRune(r)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
	return nil
}

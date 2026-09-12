// Package controllerbackup owns versioned local Controller database backups.
// Its manifest and retention model are platform-neutral; the current storage
// implementation is Ubuntu/Linux-specific.
package controllerbackup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const (
	FormatVersion uint32 = 1
	DatabaseFile         = "farm.db"
	ManifestFile         = "manifest.json"
)

type BackupID string

func ParseBackupID(value string) (BackupID, error) {
	if len(value) != len("backup_")+32 || !strings.HasPrefix(value, "backup_") {
		return "", errors.New("invalid BackupID")
	}
	for _, char := range value[len("backup_"):] {
		if char < '0' || char > '9' && (char < 'a' || char > 'f') {
			return "", errors.New("invalid BackupID")
		}
	}
	return BackupID(value), nil
}

func (id BackupID) String() string { return string(id) }
func (id BackupID) Validate() error {
	_, err := ParseBackupID(string(id))
	return err
}

type Class string

const (
	ClassHourly Class = "HOURLY"
	ClassManual Class = "MANUAL"
	ClassSafety Class = "PRE_RESTORE_SAFETY"
)

type Manifest struct {
	FormatVersion      uint32                `json:"format_version"`
	BackupID           BackupID              `json:"backup_id"`
	Class              Class                 `json:"class"`
	CreatedAt          time.Time             `json:"created_at"`
	ControllerID       identity.ControllerID `json:"controller_id"`
	FarmID             identity.FarmID       `json:"farm_id"`
	TrustFingerprint   string                `json:"trust_fingerprint"`
	ApplicationVersion string                `json:"application_version"`
	DatabaseSchema     uint32                `json:"database_schema"`
	StateRevision      uint64                `json:"state_revision"`
	DatabaseFile       string                `json:"database_file"`
	DatabaseSize       int64                 `json:"database_size"`
	DatabaseSHA256     string                `json:"database_sha256"`
}

type CreateResult struct {
	Code     farmerr.Code
	Created  bool
	Manifest Manifest
}

func validateClass(value Class) bool {
	return value == ClassHourly || value == ClassManual || value == ClassSafety
}

func validateManifest(value Manifest) error {
	if value.FormatVersion != FormatVersion || value.BackupID.Validate() != nil || !validateClass(value.Class) {
		return errors.New("unsupported backup manifest")
	}
	if value.CreatedAt.IsZero() || value.CreatedAt.Location() != time.UTC || value.ControllerID.Validate() != nil || value.FarmID.Validate() != nil || !validTrustFingerprint(value.TrustFingerprint) {
		return errors.New("invalid backup provenance")
	}
	if len(value.ApplicationVersion) == 0 || len(value.ApplicationVersion) > 64 || strings.ContainsAny(value.ApplicationVersion, "\x00\r\n") {
		return errors.New("invalid application version")
	}
	if value.DatabaseSchema == 0 || value.StateRevision == 0 || value.DatabaseFile != DatabaseFile || value.DatabaseSize <= 0 {
		return errors.New("invalid backup database metadata")
	}
	if len(value.DatabaseSHA256) != 64 {
		return errors.New("invalid backup checksum")
	}
	for _, char := range value.DatabaseSHA256 {
		if char < '0' || char > '9' && (char < 'a' || char > 'f') {
			return errors.New("invalid backup checksum")
		}
	}
	return nil
}

func validTrustFingerprint(value string) bool {
	if len(value) != len("SHA256:")+64 || !strings.HasPrefix(value, "SHA256:") {
		return false
	}
	for _, char := range value[len("SHA256:"):] {
		if char < '0' || char > '9' && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

// rejectDuplicateManifestFields makes security-sensitive manifest parsing
// stricter than encoding/json's normal last-value-wins behavior.
func rejectDuplicateManifestFields(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("manifest is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("manifest must be a JSON object")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("manifest contains a duplicate field")
		}
		seen[key] = true
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return errors.New("manifest object is not closed")
	}
	if err := decoder.Decode(&token); err != io.EOF {
		return errors.New("manifest has trailing data")
	}
	return nil
}

// Retained returns the union of the newest hourlyLimit automatic points and
// the newest dailyLimit UTC-day-separated automatic points. Manual and
// pre-restore safety backups are always retained.
func Retained(values []Manifest, hourlyLimit, dailyLimit int) map[BackupID]bool {
	result := make(map[BackupID]bool)
	ordered := append([]Manifest(nil), values...)
	sortManifests(ordered)
	hourly := 0
	days := make(map[string]bool)
	for _, item := range ordered {
		if item.Class != ClassHourly {
			result[item.BackupID] = true
			continue
		}
		if hourly < hourlyLimit {
			result[item.BackupID] = true
			hourly++
		}
		day := item.CreatedAt.UTC().Format("2006-01-02")
		if len(days) < dailyLimit && !days[day] {
			result[item.BackupID] = true
			days[day] = true
		}
	}
	return result
}

func sortManifests(values []Manifest) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0; j-- {
			left, right := values[j-1], values[j]
			if left.CreatedAt.After(right.CreatedAt) || left.CreatedAt.Equal(right.CreatedAt) && left.BackupID.String() > right.BackupID.String() {
				break
			}
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}

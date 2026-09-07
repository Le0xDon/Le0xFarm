// Package controlleridentity persists the Controller's FarmID and ControllerID.
package controlleridentity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const FileName = "identity.json"
const fileVersion uint32 = 1
const maxFileBytes = 16 * 1024

type Identity struct {
	ControllerID identity.ControllerID `json:"controller_id"`
	FarmID       identity.FarmID       `json:"farm_id"`
}

// DataDir follows Linux XDG rules. LE0X_CONTROLLER_DATA_DIR is an exact override.
func DataDir() (string, error) {
	if override := os.Getenv("LE0X_CONTROLLER_DATA_DIR"); override != "" {
		path, err := filepath.Abs(override)
		if err != nil {
			return "", storageError(farmerr.CONFIG_CONFLICT, override, err)
		}
		return path, nil
	}
	base := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			if err == nil {
				err = errors.New("home directory must be absolute")
			}
			return "", storageError(farmerr.CONFIG_CONFLICT, "HOME", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "le0xfarm", "controller"), nil
}

func storageError(code farmerr.Code, path string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		code = farmerr.PERMISSION_DENIED
	}
	return farmerr.Error{Code: code, HumanMessage: "Cannot load or persist Controller identity", Details: map[string]string{"path": path, "reason": err.Error()}, SuggestedFix: "Restore the damaged identity from a trusted backup; do not delete it to bypass an error."}
}

func notInitializedError(path string) error {
	return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "Controller is not initialized", Details: map[string]string{"path": path}, SuggestedFix: "Run le0x-controller with --init only when intentionally creating a new Controller/Farm identity."}
}

// Load reads an existing identity and never creates storage.
func Load(dir string) (Identity, error) {
	var empty Identity
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return empty, notInitializedError(dir)
	}
	if err != nil {
		return empty, storageError(farmerr.CONFIG_CONFLICT, dir, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return empty, storageError(farmerr.PERMISSION_DENIED, dir, errors.New("data directory must be a real directory with permissions 0700"))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	defer root.Close()
	id, err := read(root)
	if errors.Is(err, os.ErrNotExist) {
		return empty, notInitializedError(filepath.Join(dir, FileName))
	}
	if err != nil {
		return empty, storageError(farmerr.CONFIG_CONFLICT, filepath.Join(dir, FileName), err)
	}
	return id, nil
}

// Initialize explicitly creates a new identity. Existing identity is never replaced.
func Initialize(dir string) (Identity, error) {
	var empty Identity
	if err := os.MkdirAll(dir, 0700); err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return empty, storageError(farmerr.PERMISSION_DENIED, dir, errors.New("data directory must be a real directory with permissions 0700"))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	defer root.Close()
	if _, err := read(root); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return empty, storageError(farmerr.CONFIG_CONFLICT, filepath.Join(dir, FileName), err)
		}
		return empty, farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "Controller is already initialized", Details: map[string]string{"path": filepath.Join(dir, FileName)}, SuggestedFix: "Remove --init and use the existing Controller/Farm identity."}
	}
	if _, err := root.Lstat(FileName); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = errors.New("identity path already exists but cannot be read")
		}
		return empty, storageError(farmerr.CONFIG_CONFLICT, filepath.Join(dir, FileName), err)
	}
	controllerID, err := identity.NewControllerID()
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	farmID, err := identity.NewFarmID()
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	id := Identity{ControllerID: controllerID, FarmID: farmID}
	data, err := json.MarshalIndent(struct {
		SchemaVersion uint32 `json:"schema_version"`
		Identity
	}{fileVersion, id}, "", "  ")
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	tempName := ".identity-" + controllerID.String() + ".tmp"
	temp, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	defer root.Remove(tempName)
	err = temp.Chmod(0600)
	if err == nil {
		_, err = temp.Write(append(data, '\n'))
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	if err := root.Link(tempName, FileName); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
		}
		existing, readErr := read(root)
		if readErr != nil {
			return empty, storageError(farmerr.CONFIG_CONFLICT, filepath.Join(dir, FileName), readErr)
		}
		return existing, nil
	}
	if err := root.Remove(tempName); err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	directory, err := root.Open(".")
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	err = directory.Sync()
	closeErr = directory.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	return id, nil
}

func read(root *os.Root) (Identity, error) {
	var id Identity
	file, err := root.OpenFile(FileName, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return id, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return id, err
	}
	if !info.Mode().IsRegular() {
		return id, errors.New("identity must be a regular file")
	}
	if info.Mode().Perm() != 0600 {
		return id, fmt.Errorf("identity permissions must be 0600, got %04o", info.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return id, err
	}
	if len(data) > maxFileBytes {
		return id, errors.New("identity file exceeds size limit")
	}
	return decode(data)
}

func decode(data []byte) (Identity, error) {
	var id Identity
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return id, err
	}
	if token != json.Delim('{') {
		return id, errors.New("identity must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return id, err
		}
		key, ok := token.(string)
		if !ok {
			return id, errors.New("invalid identity field")
		}
		if _, exists := fields[key]; exists {
			return id, fmt.Errorf("duplicate identity field %q", key)
		}
		if key != "schema_version" && key != "controller_id" && key != "farm_id" {
			return id, fmt.Errorf("unknown identity field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return id, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return id, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return id, errors.New("trailing data after identity")
	}
	versionRaw, ok := fields["schema_version"]
	if !ok {
		return id, errors.New("schema_version is missing")
	}
	var version uint32
	if err := json.Unmarshal(versionRaw, &version); err != nil {
		return id, err
	}
	if version != fileVersion {
		return id, fmt.Errorf("unsupported identity schema version %d", version)
	}
	controllerRaw, ok := fields["controller_id"]
	if !ok {
		return id, errors.New("controller_id is missing")
	}
	if err := json.Unmarshal(controllerRaw, &id.ControllerID); err != nil {
		return id, err
	}
	farmRaw, ok := fields["farm_id"]
	if !ok {
		return id, errors.New("farm_id is missing")
	}
	if err := json.Unmarshal(farmRaw, &id.FarmID); err != nil {
		return id, err
	}
	if err := id.ControllerID.Validate(); err != nil {
		return Identity{}, err
	}
	if err := id.FarmID.Validate(); err != nil {
		return Identity{}, err
	}
	return id, nil
}

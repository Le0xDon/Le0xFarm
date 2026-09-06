// Package agentidentity persists local Agent and Host identities independently of discovery.
package agentidentity

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
const fileVersion = 1
const maxFileBytes = 16 * 1024

type Identity struct {
	HostID  identity.HostID  `json:"host_id"`
	AgentID identity.AgentID `json:"agent_id"`
}

// DataDir follows Linux XDG rules. Relative XDG_DATA_HOME is ignored.
// LE0X_DATA_DIR is an explicit override and may be relative to the working directory.
func DataDir() (string, error) {
	if override := os.Getenv("LE0X_DATA_DIR"); override != "" {
		path, err := filepath.Abs(override)
		if err != nil {
			return "", storageError(farmerr.CONFIG_CONFLICT, override, err)
		}
		return path, nil
	}
	base := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", storageError(farmerr.CONFIG_CONFLICT, "HOME", err)
		}
		if !filepath.IsAbs(home) {
			return "", storageError(farmerr.CONFIG_CONFLICT, "HOME", errors.New("home directory must be absolute"))
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "le0xfarm", "agent"), nil
}

func storageError(code farmerr.Code, path string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		code = farmerr.PERMISSION_DENIED
	}
	return farmerr.Error{Code: code, HumanMessage: "Cannot load or persist local Agent identity",
		Details:      map[string]string{"path": path, "reason": err.Error()},
		SuggestedFix: "Check the data path and permissions; restore a damaged identity from a trusted backup. Do not delete it to bypass an error."}
}

// LoadOrCreate creates identity only when the file is absent, never on a read error.
// A synced temporary file is published with an atomic no-replace hard link.
// Concurrent initializers converge on the winner's identity without overwriting it.
func LoadOrCreate(dir string) (Identity, error) {
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
	if id, err := read(root); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return empty, storageError(farmerr.CONFIG_CONFLICT, filepath.Join(dir, FileName), err)
		}
		return id, nil
	}
	// Reject dangling symlinks too: an existing directory entry is not a first run.
	if _, err := root.Lstat(FileName); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = errors.New("identity path already exists but cannot be read")
		}
		return empty, storageError(farmerr.CONFIG_CONFLICT, filepath.Join(dir, FileName), err)
	}
	host, err := identity.NewHostID()
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	agent, err := identity.NewAgentID()
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	id := Identity{HostID: host, AgentID: agent}
	data, err := json.MarshalIndent(struct {
		Version uint32 `json:"schema_version"`
		Identity
	}{fileVersion, id}, "", "  ")
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	tempName := ".identity-" + agent.String() + ".tmp"
	temp, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return empty, storageError(farmerr.INTERNAL_ERROR, dir, err)
	}
	defer root.Remove(tempName)
	// Chmod makes the final mode exact even with an unusually restrictive umask.
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
		token, err := decoder.Token()
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
		if key != "schema_version" && key != "host_id" && key != "agent_id" {
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
	var version uint32
	if err := json.Unmarshal(fields["schema_version"], &version); err != nil {
		return id, err
	}
	if version != fileVersion {
		return id, fmt.Errorf("unsupported identity schema version %d", version)
	}
	if err := json.Unmarshal(fields["host_id"], &id.HostID); err != nil {
		return Identity{}, err
	}
	if err := json.Unmarshal(fields["agent_id"], &id.AgentID); err != nil {
		return Identity{}, err
	}
	if err := id.HostID.Validate(); err != nil {
		return Identity{}, err
	}
	if err := id.AgentID.Validate(); err != nil {
		return Identity{}, err
	}
	return id, nil
}

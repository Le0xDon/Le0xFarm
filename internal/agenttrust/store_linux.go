// Package agenttrust stores the Controller identity trusted by an Agent.
package agenttrust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const FileName = "controller.json"
const schemaVersion uint32 = 1

type Binding struct {
	ControllerID identity.ControllerID `json:"controller_id"`
	FarmID       identity.FarmID       `json:"farm_id"`
}
type disk struct {
	SchemaVersion uint32 `json:"schema_version"`
	Binding
}

func Load(dir string) (Binding, error) {
	var b Binding
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return b, os.ErrNotExist
	}
	if err != nil {
		return b, invalid(err)
	}
	if info, statErr := os.Stat(filepath.Join(dir, FileName)); statErr == nil && info.Mode().Perm() != 0600 {
		return b, invalid(fmt.Errorf("trust file permissions must be 0600"))
	}
	var d disk
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&d); err != nil {
		return b, invalid(err)
	}
	if d.SchemaVersion != schemaVersion {
		return b, invalid(fmt.Errorf("schema_version=%d", d.SchemaVersion))
	}
	if err = d.ControllerID.Validate(); err != nil {
		return b, invalid(err)
	}
	if err = d.FarmID.Validate(); err != nil {
		return b, invalid(err)
	}
	return d.Binding, nil
}
func Save(dir string, b Binding) error {
	if err := b.ControllerID.Validate(); err != nil {
		return invalid(err)
	}
	if err := b.FarmID.Validate(); err != nil {
		return invalid(err)
	}
	if old, err := Load(dir); err == nil {
		if old != b {
			return farmerr.Error{Code: farmerr.CONTROLLER_IDENTITY_MISMATCH, HumanMessage: "Controller identity differs from trusted binding"}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return invalid(err)
	}
	_ = os.Chmod(dir, 0700)
	data, _ := json.MarshalIndent(disk{schemaVersion, b}, "", "  ")
	tmp, err := os.CreateTemp(dir, ".controller-")
	if err != nil {
		return invalid(err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0600)
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return invalid(err)
	}
	if err = os.Rename(name, filepath.Join(dir, FileName)); err != nil {
		return invalid(err)
	}
	if f, e := os.Open(dir); e == nil {
		e = f.Sync()
		_ = f.Close()
		if e != nil {
			return invalid(e)
		}
	}
	return nil
}
func invalid(err error) error {
	return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "Invalid Controller trust binding", Details: map[string]string{"reason": err.Error()}}
}

// Package controllertrust persists the Controller's paired Agent identities.
package controllertrust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const FileName = "paired_agents.json"
const schemaVersion uint32 = 1

type AgentRecord struct {
	AgentID  identity.AgentID `json:"agent_id"`
	HostID   identity.HostID  `json:"host_id"`
	PairedAt time.Time        `json:"paired_at"`
}
type disk struct {
	SchemaVersion uint32                `json:"schema_version"`
	ControllerID  identity.ControllerID `json:"controller_id"`
	FarmID        identity.FarmID       `json:"farm_id"`
	Agents        []AgentRecord         `json:"agents"`
}
type Store struct {
	dir          string
	controllerID identity.ControllerID
	farmID       identity.FarmID
	mu           sync.Mutex
	agents       map[identity.AgentID]AgentRecord
}

func Open(dir string, controllerID identity.ControllerID, farmID identity.FarmID) (*Store, error) {
	if err := controllerID.Validate(); err != nil {
		return nil, conflict("invalid ControllerID", err)
	}
	if err := farmID.Validate(); err != nil {
		return nil, conflict("invalid FarmID", err)
	}
	st := &Store{dir: dir, controllerID: controllerID, farmID: farmID, agents: make(map[identity.AgentID]AgentRecord)}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, conflict("cannot read paired agents", err)
	}
	if info, statErr := os.Stat(filepath.Join(dir, FileName)); statErr == nil && info.Mode().Perm() != 0600 {
		return nil, conflict("paired agents file must have permissions 0600", nil)
	}
	var d disk
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, conflict("invalid paired agents JSON", err)
	}
	if d.SchemaVersion != schemaVersion {
		return nil, conflict("unsupported paired agents schema", fmt.Errorf("schema_version=%d", d.SchemaVersion))
	}
	if d.ControllerID != controllerID || d.FarmID != farmID {
		return nil, conflict("paired agents belong to another Controller/Farm", nil)
	}
	byHost := map[identity.HostID]identity.AgentID{}
	for _, a := range d.Agents {
		if err := a.AgentID.Validate(); err != nil {
			return nil, conflict("invalid paired AgentID", err)
		}
		if err := a.HostID.Validate(); err != nil {
			return nil, conflict("invalid paired HostID", err)
		}
		if old, ok := st.agents[a.AgentID]; ok && old.HostID != a.HostID {
			return nil, conflict("duplicate AgentID with different HostID", nil)
		}
		if old, ok := byHost[a.HostID]; ok && old != a.AgentID {
			return nil, conflict("duplicate HostID with different AgentID", nil)
		}
		st.agents[a.AgentID] = a
		byHost[a.HostID] = a.AgentID
	}
	return st, nil
}

func (s *Store) Find(agentID identity.AgentID) (AgentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[agentID]
	return a, ok
}
func (s *Store) Pair(agentID identity.AgentID, hostID identity.HostID) error {
	if err := agentID.Validate(); err != nil {
		return conflict("invalid AgentID", err)
	}
	if err := hostID.Validate(); err != nil {
		return conflict("invalid HostID", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agents {
		if a.AgentID == agentID {
			if a.HostID != hostID {
				return conflict("AgentID is bound to another HostID", nil)
			}
			return nil
		}
		if a.HostID == hostID {
			return conflict("HostID is bound to another AgentID", nil)
		}
	}
	record := AgentRecord{AgentID: agentID, HostID: hostID, PairedAt: time.Now().UTC()}
	s.agents[agentID] = record
	if err := s.persistLocked(); err != nil {
		delete(s.agents, agentID)
		return err
	}
	return nil
}
func (s *Store) persistLocked() error {
	list := make([]AgentRecord, 0, len(s.agents))
	for _, a := range s.agents {
		list = append(list, a)
	}
	data, err := json.MarshalIndent(disk{schemaVersion, s.controllerID, s.farmID, list}, "", "  ")
	if err != nil {
		return conflict("cannot encode paired agents", err)
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return conflict("cannot create trust directory", err)
	}
	_ = os.Chmod(s.dir, 0700)
	tmp, err := os.CreateTemp(s.dir, ".paired-")
	if err != nil {
		return conflict("cannot create trust temporary", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return conflict("cannot persist paired agents", err)
	}
	if err = os.Rename(name, filepath.Join(s.dir, FileName)); err != nil {
		return conflict("cannot publish paired agents", err)
	}
	f, e := os.Open(s.dir)
	if e == nil {
		e = f.Sync()
		_ = f.Close()
	}
	if e != nil {
		return conflict("cannot sync trust directory", e)
	}
	return nil
}
func conflict(msg string, err error) error {
	d := map[string]string{}
	if err != nil {
		d["reason"] = err.Error()
	}
	return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: msg, Details: d}
}

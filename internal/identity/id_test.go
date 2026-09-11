package identity_test

import (
	"encoding"
	"encoding/json"
	"strings"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

type identifier interface {
	comparable
	String() string
	Validate() error
	MarshalText() ([]byte, error)
}

func checkID[T identifier](t *testing.T, prefix string, newID func() (T, error), parse func(string) (T, error)) {
	t.Helper()
	var zero T
	if zero.Validate() == nil {
		t.Fatal("zero ID must be invalid")
	}
	if _, err := zero.MarshalText(); err == nil {
		t.Fatal("zero ID must not marshal")
	}
	if _, err := json.Marshal(zero); err == nil {
		t.Fatal("zero ID must not marshal to JSON")
	}
	seen := make(map[T]bool)
	for i := 0; i < 1000; i++ {
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		if err := id.Validate(); err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatal("duplicate ID")
		}
		seen[id] = true
		parsed, err := parse(id.String())
		if err != nil || parsed != id {
			t.Fatalf("round trip failed: %v", err)
		}
		encoded, err := json.Marshal(id)
		if err != nil || string(encoded) != `"`+id.String()+`"` {
			t.Fatalf("JSON ID: %s, %v", encoded, err)
		}
		var decoded T
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != id {
			t.Fatalf("JSON round trip failed: %v", err)
		}
		raw, err := id.MarshalText()
		if err != nil {
			t.Fatal(err)
		}
		var fromText T
		if err := any(&fromText).(encoding.TextUnmarshaler).UnmarshalText(raw); err != nil || fromText != id {
			t.Fatalf("text round trip failed: %v", err)
		}
		mapJSON, err := json.Marshal(map[T]string{id: "device"})
		if err != nil {
			t.Fatal(err)
		}
		var decodedMap map[T]string
		if err := json.Unmarshal(mapJSON, &decodedMap); err != nil || decodedMap[id] != "device" || len(decodedMap) != 1 {
			t.Fatalf("map key round trip failed: %v", err)
		}
	}
	for _, bad := range []string{"", prefix + "_", prefix + "_" + strings.Repeat("0", 31), prefix + "_" + strings.Repeat("0", 33), prefix + "_" + strings.Repeat("A", 32), prefix + "_" + strings.Repeat("z", 32), "wrong_" + strings.Repeat("0", 32), " " + prefix + "_" + strings.Repeat("0", 32)} {
		original, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		target := original
		if err := any(&target).(encoding.TextUnmarshaler).UnmarshalText([]byte(bad)); err == nil || target != original {
			t.Errorf("invalid text %q must fail without mutation", bad)
		}
		encoded, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &target); err == nil || target != original {
			t.Errorf("invalid JSON ID %q must fail without mutation", bad)
		}
		id, err := parse(bad)
		if err == nil || id != zero {
			t.Errorf("accepted invalid ID %q", bad)
		}
	}
	canonical := prefix + "_0123456789abcdef0123456789abcdef"
	id, err := parse(canonical)
	if err != nil || id.String() != canonical {
		t.Fatalf("canonical ID rejected: %v", err)
	}
}

func TestIDs(t *testing.T) {
	t.Run("Package", func(t *testing.T) { checkID(t, "package", identity.NewPackageID, identity.ParsePackageID) })
	t.Run("Pool", func(t *testing.T) { checkID(t, "pool", identity.NewPoolID, identity.ParsePoolID) })
	t.Run("Wallet", func(t *testing.T) { checkID(t, "wallet", identity.NewWalletID, identity.ParseWalletID) })
	t.Run("Farm", func(t *testing.T) { checkID(t, "farm", identity.NewFarmID, identity.ParseFarmID) })
	t.Run("Controller", func(t *testing.T) { checkID(t, "controller", identity.NewControllerID, identity.ParseControllerID) })
	t.Run("Host", func(t *testing.T) { checkID(t, "host", identity.NewHostID, identity.ParseHostID) })
	t.Run("Agent", func(t *testing.T) { checkID(t, "agent", identity.NewAgentID, identity.ParseAgentID) })
	t.Run("Noda", func(t *testing.T) { checkID(t, "noda", identity.NewNodaID, identity.ParseNodaID) })
	t.Run("Device", func(t *testing.T) { checkID(t, "device", identity.NewDeviceID, identity.ParseDeviceID) })
	t.Run("Profile", func(t *testing.T) { checkID(t, "profile", identity.NewProfileID, identity.ParseProfileID) })
	t.Run("Execution", func(t *testing.T) { checkID(t, "execution", identity.NewExecutionID, identity.ParseExecutionID) })
	t.Run("Service", func(t *testing.T) { checkID(t, "service", identity.NewServiceID, identity.ParseServiceID) })
	t.Run("Workload", func(t *testing.T) { checkID(t, "workload", identity.NewWorkloadID, identity.ParseWorkloadID) })
}

func TestSameHostnameHasIndependentHostIDs(t *testing.T) {
	first, err := identity.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := identity.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	hosts := []model.Host{{HostID: first, Hostname: "worker"}, {HostID: second, Hostname: "worker"}}
	if hosts[0].HostID == hosts[1].HostID {
		t.Fatal("same hostname must not determine HostID")
	}
	hosts[0].Hostname = "renamed"
	if hosts[0].HostID != first {
		t.Fatal("rename changed identity")
	}
	if _, err := identity.ParseHostID("agent_0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("accepted another ID type")
	}
}

func TestDeterministicIncidentIDTextRoundTrip(t *testing.T) {
	value := "incident_0123456789abcdef0123456789abcdef"
	id, err := identity.ParseIncidentID(value)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	var decoded identity.IncidentID
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != id {
		t.Fatalf("round trip=%s err=%v", decoded, err)
	}
	if err := json.Unmarshal([]byte(`"incident_BAD"`), &decoded); err == nil || decoded != id {
		t.Fatal("invalid IncidentID mutated value")
	}
}

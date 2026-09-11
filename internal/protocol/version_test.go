package protocol

import "testing"

func TestM43Versions(t *testing.T) {
	if CurrentProtocolVersion != 4 {
		t.Fatalf("ProtocolVersion=%d", CurrentProtocolVersion)
	}
	if CurrentSchemaVersion != 1 {
		t.Fatalf("SchemaVersion=%d", CurrentSchemaVersion)
	}
}

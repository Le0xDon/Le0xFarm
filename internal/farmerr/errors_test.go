package farmerr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	farmerr "github.com/le0xdon/le0xfarm/internal/farmerr"
)

func TestStructuredError(t *testing.T) {
	original := farmerr.Error{
		Code:         farmerr.MISSING_POOL,
		HumanMessage: "Pool is required",
		Details:      map[string]string{"profile": "example"},
		SuggestedFix: "Set a pool URL",
		LogsRef:      "logs/example",
	}
	var structured farmerr.Error
	if !errors.As(fmt.Errorf("profile invalid: %w", original), &structured) || structured.Code != farmerr.MISSING_POOL {
		t.Fatal("structured error lost during wrapping")
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"code", "human_message", "details", "suggested_fix", "logs_ref"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("missing field %s", field)
		}
	}
	if original.Error() != "MISSING_POOL: Pool is required" {
		t.Fatal(original.Error())
	}
	if (farmerr.Error{Code: farmerr.INTERNAL_ERROR}).Error() != "INTERNAL_ERROR" {
		t.Fatal("missing code-only message")
	}
}

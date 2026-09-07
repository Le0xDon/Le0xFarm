package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestControllerCLIRefusesPlaintextByDefault(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--listen", "127.0.0.1:0"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "--insecure-dev") {
		t.Fatalf("missing explicit development warning: %s", errOut.String())
	}
}

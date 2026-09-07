package controllernet

import (
	"errors"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
)

func TestPairingWindowToken(t *testing.T) {
	now := time.Unix(100, 0)
	w, token, _, err := NewPairingWindow(time.Minute, func() time.Time { return now })
	if err != nil || len(token) < 43 {
		t.Fatalf("token: %v", err)
	}
	called := 0
	if err := w.Use(token, func() error { called++; return nil }); err != nil || called != 1 {
		t.Fatalf("use: %v", err)
	}
	if err := w.Use(token, func() error { return nil }); codeOf(err) != farmerr.PAIRING_TOKEN_INVALID {
		t.Fatalf("single use: %v", err)
	}
}

func TestPairingWindowRejectsWrongToken(t *testing.T) {
	w, _, _, err := NewPairingWindow(time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Use("wrong", func() error { t.Fatal("persist called"); return nil }); codeOf(err) != farmerr.PAIRING_TOKEN_INVALID {
		t.Fatalf("error=%v", err)
	}
}

func TestPairingWindowExpiryAndPersistenceFailure(t *testing.T) {
	now := time.Unix(0, 0)
	w, token, _, _ := NewPairingWindow(time.Second, func() time.Time { return now })
	if err := w.Use(token, func() error { return errors.New("persist") }); err == nil {
		t.Fatal("expected persistence failure")
	}
	now = now.Add(2 * time.Second)
	if err := w.Use(token, func() error { return nil }); codeOf(err) != farmerr.PAIRING_TOKEN_EXPIRED {
		t.Fatalf("expiry: %v", err)
	}
}

func codeOf(err error) farmerr.Code { c, _ := farmerr.CodeOf(err); return c }

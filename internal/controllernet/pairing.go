package controllernet

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
)

type PairingWindow struct {
	mu      sync.Mutex
	digest  [32]byte
	expires time.Time
	active  bool
	now     func() time.Time
}

func NewPairingWindow(ttl time.Duration, now func() time.Time) (*PairingWindow, string, time.Time, error) {
	if ttl <= 0 {
		return nil, "", time.Time{}, errors.New("pairing TTL must be positive")
	}
	if now == nil {
		now = time.Now
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	w := &PairingWindow{digest: sha256.Sum256([]byte(token)), expires: now().Add(ttl), active: true, now: now}
	return w, token, w.expires, nil
}
func (w *PairingWindow) Use(token string, persist func() error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return farmerr.Error{Code: farmerr.PAIRING_TOKEN_INVALID, HumanMessage: "Enrollment token is invalid or already used"}
	}
	if !w.now().Before(w.expires) {
		w.active = false
		return farmerr.Error{Code: farmerr.PAIRING_TOKEN_EXPIRED, HumanMessage: "Enrollment token has expired"}
	}
	got := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(got[:], w.digest[:]) != 1 {
		return farmerr.Error{Code: farmerr.PAIRING_TOKEN_INVALID, HumanMessage: "Enrollment token is invalid"}
	}
	if err := persist(); err != nil {
		return err
	}
	w.active = false
	return nil
}

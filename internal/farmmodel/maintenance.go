package farmmodel

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/le0xdon/le0xfarm/internal/identity"
)

// MaintenanceHold is persistent Controller-side suppression state. It does
// not change DesiredWorkload intent and is not evidence of electrical safety.
type MaintenanceHold struct {
	HostID    identity.HostID
	Active    bool
	Revision  uint64
	Reason    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func ValidateMaintenanceReason(reason string) error {
	if len(reason) > 512 || !utf8.ValidString(reason) || strings.ContainsAny(reason, "\x00\r\n") {
		return invalid("invalid Maintenance Hold reason", nil)
	}
	return nil
}

package imscore

// [dev] full IMS state snapshot every devSnapshotEvery inbound-stats ticks
// (5 minutes), so a log excerpt around a failure shows the state it happened
// in. Identities in the status are masked by logging.DevRedactText.

import (
	"encoding/json"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

const devSnapshotEvery = 10

func (s *Service) devLogStateSnapshot(tick uint64) {
	if s == nil || tick%devSnapshotEvery != 1 {
		return
	}
	status := s.StatusCurrent()
	if status == nil {
		return
	}
	encoded, err := json.Marshal(map[string]any{"status": status.ToMap(), "network": s.networkDiagnostics()})
	if err != nil {
		logging.DevLog("IMS state snapshot unavailable", "device", s.DeviceID(), "err", err)
		return
	}
	logging.DevLog("IMS state snapshot", "device", s.DeviceID(), "tick", tick,
		"state", logging.DevRedactText(string(encoded)))
}

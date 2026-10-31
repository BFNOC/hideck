package imscore

import (
	"time"
)

// Vodafone DE resets port-s 8-15 s after every REGISTER 200, right after the
// reg-event NOTIFY (24/24 closes on 10-31, hideck#90), and answered the
// REGISTER sent straight after each reset with 403. TS 33.203 7.1 (NOTE 6) and
// TS 24.229 4.2A NOTE 2 let the P-CSCF close its connection to port_us and set
// up a new one before its next request, when neither NAT, TLS nor the multiple
// registration mechanism is used, so a negotiated outbound flow keeps the usual
// recovery. Whether this P-CSCF reopens port-s for an MT request is not known,
// so the wait is short and SMS reception is not reported ready meanwhile.
// After the cap the usual port-s recovery REGISTER runs; a REGISTER that
// succeeds within the cap starts the usual port-s validation instead.
const vodafoneDEPortSResetAwaitCap = 3 * time.Minute

func vodafoneDEPortSResetAwaiting(preset string, session portSSessionSnapshot, outbound bool, now time.Time) bool {
	return preset == vodafoneDECarrierPresetID && !outbound && session.lastCloseKind == portSClosePeerReset &&
		!session.closedAt.IsZero() && now.Sub(session.closedAt) < vodafoneDEPortSResetAwaitCap
}

// vodafoneDEPortSAwaitActive reports a wait that defers recovery but has not
// shown that the P-CSCF delivers over a new port-s, so it does not count for
// SMS readiness.
func (s *Service) vodafoneDEPortSAwaitActive() bool {
	return s != nil && vodafoneDEPortSResetAwaiting(s.devCarrierPreset(), s.capturePortSSession(),
		s.sipOutboundFlow.Load(), time.Now())
}

// scheduleVodafoneDEPortSAwaitCap arms the recovery watch for the end of the
// wait. A peer reconnect before then cancels it.
func (s *Service) scheduleVodafoneDEPortSAwaitCap() {
	if !s.vodafoneDEPortSAwaitActive() {
		return
	}
	session := s.capturePortSSession()
	deadline := session.closedAt.Add(vodafoneDEPortSResetAwaitCap)
	devLog("IMS port-s reset; deferring the port-s recovery REGISTER",
		"device", s.DeviceID(), "port_s_generation", session.generation,
		"cap", vodafoneDEPortSResetAwaitCap, "register_at", deadline,
		"lifetime_before_reset", session.closedAt.Sub(session.openedAt).Round(time.Millisecond),
		"sms_ready", false)
	s.schedulePortSReconnectWatchAt(deadline)
}

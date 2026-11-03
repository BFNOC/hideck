package imscore

import (
	"fmt"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

// Per-SIM voicemail indication settings (hideck#97). Auto follows the carrier
// preset; on ignores the preset; off never subscribes.
const (
	MWISubscriptionAuto = "auto"
	MWISubscriptionOn   = "on"
	MWISubscriptionOff  = "off"
)

// MWISubscriptionPolicyStore supplies the per-SIM MWI setting and remembers
// identities whose network explicitly rejected the message-summary package.
// A remembered rejection survives restarts, reconnects and deregistration;
// only the user clears it, by turning the setting on again.
type MWISubscriptionPolicyStore interface {
	LoadMWISubscriptionPolicy(iccid, identity string) (mode string, rejectedStatus int, err error)
	SaveMWISubscriptionRejection(iccid, identity string, status int) error
}

// mwiSubscriptionBlockedReason is checked before every SUBSCRIBE(mwi) that
// starts or refreshes the usage. Unsubscribe is not gated.
func (s *Service) mwiSubscriptionBlockedReason() string {
	mode, rejected := MWISubscriptionAuto, 0
	if store := s.cfg.MWIPolicyStore; store != nil {
		var err error
		mode, rejected, err = store.LoadMWISubscriptionPolicy(s.cfg.ICCID, s.mwiPolicyIdentity())
		if err != nil {
			// Without the setting we cannot tell whether the user turned MWI
			// off, and on some networks the request itself costs port-s.
			return fmt.Sprintf("MWI policy unavailable: %v", err)
		}
	}
	switch {
	case mode == MWISubscriptionOff:
		return "SIM setting disables MWI subscription"
	case rejected != 0:
		return fmt.Sprintf("network rejected MWI with status %d", rejected)
	case mode != MWISubscriptionOn && s.cfg.IMSRegisterTemplate.DisableMWISubscription:
		return "carrier policy disables MWI subscription"
	}
	return ""
}

func (s *Service) mwiPolicyIdentity() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.subscriptionBinding.identity
}

// rememberMWIRejectionLocked records a network-side 405/489. Self-routed 405s
// never reach here.
func (s *Service) rememberMWIRejectionLocked(binding subscriptionRegistration, status int) {
	store := s.cfg.MWIPolicyStore
	if store == nil {
		return
	}
	if err := store.SaveMWISubscriptionRejection(s.cfg.ICCID, binding.identity, status); err != nil {
		logging.WarnRate("ims-mwi-rejection-save-"+s.DeviceID(), "IMS MWI rejection was not saved; the next registration will probe again",
			"device", s.DeviceID(), "status", status, "err", err)
		return
	}
	logging.Info("IMS MWI rejection saved; SUBSCRIBE(mwi) stays off until the SIM setting is turned on",
		"device", s.DeviceID(), "status", status)
}

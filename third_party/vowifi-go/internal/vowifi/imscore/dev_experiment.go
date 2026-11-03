package imscore

// [dev] hideck#90: experiments for Vodafone DE port-s resets.
// HIDECK_DEV90_EXPERIMENT selects one change or a comma-separated set, applied
// only to the vodafone_de_26202 preset; empty or "baseline" keeps today's
// behaviour, and any unknown name runs the whole set as the baseline.
// Remove before a PR.

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	devExperimentEnv    = "HIDECK_DEV90_EXPERIMENT"
	devExperimentPreset = "vodafone_de_26202"

	devExperimentNoOptions        = "no-options"
	devExperimentNoAlias          = "no-alias"
	devExperimentViaPortC         = "via-port-c"
	devExperimentViaPortS         = "via-port-s"
	devExperimentNoOutbound       = "no-outbound"
	devExperimentNoPortSKeepalive = "no-port-s-keepalive"
	devExperimentNoRegisterRoute  = "no-register-route"
	devExperimentNoFeatureCaps    = "no-feature-caps"
	devExperimentRecoveryJitter   = "recovery-jitter"
	devExperimentNotifyDialog     = "notify-dialog"
	devExperimentNoMWI            = "no-mwi"
	devExperimentDelayMWI         = "delay-mwi"
	devExperimentBaseline         = "baseline"

	// recovery-jitter delays the port-s recovery REGISTER after a peer reset
	// by a random time in this range, to tell whether the next reset follows
	// our REGISTER or a fixed P-CSCF cycle, and how long after a reset the
	// P-CSCF still answers: it sent 403 at once on 11-02 and nothing 180 s
	// later in the await run (10-31).
	devRecoveryJitterMin = 2 * time.Second
	devRecoveryJitterMax = 60 * time.Second
)

var devKnownExperiments = []string{
	devExperimentNoOptions, devExperimentNoAlias, devExperimentViaPortC, devExperimentViaPortS,
	devExperimentNoOutbound, devExperimentNoPortSKeepalive, devExperimentNoRegisterRoute,
	devExperimentNoFeatureCaps, devExperimentRecoveryJitter, devExperimentNotifyDialog, devExperimentNoMWI,
	devExperimentDelayMWI,
}

// devMWIDelay is how long delay-mwi holds SUBSCRIBE(mwi) back after a
// registration. no-mwi had no port-s reset on 11-02, while baseline port-s
// reset 14-28 s after opening: a reset right after a late SUBSCRIBE(mwi)
// points at the subscription, one at the usual 14-28 s points elsewhere.
var devMWIDelay = 120 * time.Second

// devExperiments returns the active experiments, sorted, or nil for the
// baseline.
func (s *Service) devExperiments() []string {
	if s.devCarrierPreset() != devExperimentPreset {
		return nil
	}
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(devExperimentEnv)))
	var active []string
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" || name == devExperimentBaseline || slices.Contains(active, name) {
			continue
		}
		if !slices.Contains(devKnownExperiments, name) {
			s.dev.experimentUnknown.Do(func() {
				devLog("IMS experiment not recognised; running the baseline", "device", s.DeviceID(),
					"value", raw, "unknown", name)
			})
			return nil
		}
		active = append(active, name)
	}
	slices.Sort(active)
	return active
}

func (s *Service) devExperimentHas(name string) bool {
	return slices.Contains(s.devExperiments(), name)
}

// devExperimentName is the value the [dev] REGISTER context logs.
func (s *Service) devExperimentName() string {
	if active := s.devExperiments(); len(active) > 0 {
		return strings.Join(active, ",")
	}
	return devExperimentBaseline
}

// devRecoveryJitter returns the delay before the port-s recovery REGISTER
// after a peer reset when recovery-jitter is on.
func (s *Service) devRecoveryJitter() (time.Duration, bool) {
	if !s.devExperimentHas(devExperimentRecoveryJitter) ||
		s.capturePortSSession().lastCloseKind != portSClosePeerReset {
		return 0, false
	}
	delay := devRecoveryJitterMin + rand.N(devRecoveryJitterMax-devRecoveryJitterMin)
	devLog("IMS experiment delays the port-s recovery REGISTER", "device", s.DeviceID(),
		"delay", delay.Round(time.Millisecond), "port_s_generation", s.capturePortSSession().generation)
	return delay, true
}

// devAwaitMWI is the delay-mwi experiment. It reports false, and the caller
// sends nothing, when the service stops or the registration changes during the
// wait; the new registration starts its own delayed SUBSCRIBE(mwi).
func (s *Service) devAwaitMWI() bool {
	if !s.devExperimentHas(devExperimentDelayMWI) {
		return true
	}
	s.mu.Lock()
	before := s.subscriptionContextLocked()
	s.mu.Unlock()
	devLog("IMS experiment delays SUBSCRIBE(mwi)", "device", s.DeviceID(),
		"delay", devMWIDelay, "subscription_generation", before.generation)
	timer := time.NewTimer(devMWIDelay)
	defer timer.Stop()
	select {
	case <-s.stop:
		devLog("IMS experiment drops the delayed SUBSCRIBE(mwi): service stopped", "device", s.DeviceID())
		return false
	case <-timer.C:
	}
	s.mu.Lock()
	after := s.subscriptionContextLocked()
	s.mu.Unlock()
	if after != before {
		devLog("IMS experiment drops the delayed SUBSCRIBE(mwi): registration changed", "device", s.DeviceID(),
			"subscription_generation", before.generation, "now", after.generation)
		return false
	}
	devLog("IMS experiment sends the delayed SUBSCRIBE(mwi)", "device", s.DeviceID(),
		"subscription_generation", after.generation)
	return true
}

// devNotifyDialogResponse is the notify-dialog experiment. RFC 6665 §4.4.1
// creates the subscription dialog when the NOTIFY transaction completes, and
// RFC 3261 §12.1.1 says a response that establishes a dialog MUST copy every
// Record-Route of the request in order and MUST add a Contact. Our 200 to
// NOTIFY carries neither, while Vodafone DE record-routes every NOTIFY.
func (s *Service) devNotifyDialogResponse(raw, response string) string {
	if !strings.HasPrefix(response, "SIP/2.0 2") || !s.devExperimentHas(devExperimentNotifyDialog) {
		return response
	}
	contact := ""
	s.mu.Lock()
	route := s.registeredSIPRouteLocked()
	registered := s.regSession != nil
	if registered && route.serverAddress != "" {
		if user := firstNonBlank(s.regSession.contactUser, contactUser(s.cfg)); user != "" {
			uri, _ := registeredVoiceContact(s.cfg, user, route.serverAddress)
			contact = uri + ";transport=tcp"
		}
	}
	s.mu.Unlock()
	if contact == "" {
		devLog("IMS experiment has no registered Contact for the NOTIFY 200", "device", s.DeviceID(),
			"server_address", route.serverAddress, "registered", registered)
	}
	return devAddDialogHeaders(raw, response, contact)
}

// devAddDialogHeaders inserts Contact and the request's Record-Route lines,
// verbatim and in order, ahead of Content-Length. The raw text is copied
// because sipgo's parsed Record-Route drops parameters after the '>', which
// §12.1.1 says must be kept.
func devAddDialogHeaders(request, response, contact string) string {
	var extra strings.Builder
	if contact != "" {
		fmt.Fprintf(&extra, "Contact: <%s>\r\n", contact)
	}
	if routes := devSIPHeaders(string(unfoldSIPHeaders([]byte(request))))["record-route"]; routes != "" {
		for _, value := range strings.Split(routes, "\n") {
			fmt.Fprintf(&extra, "Record-Route: %s\r\n", value)
		}
	}
	at := strings.Index(response, "\r\nContent-Length:")
	if extra.Len() == 0 || at < 0 {
		return response
	}
	return response[:at+2] + extra.String() + response[at+2:]
}

type devKeepaliveDisabler interface{ DisableKeepAlive() error }

// devDisablePortSKeepalive turns off the TCP keepalive the netstack enables
// on every accepted IMS connection.
func (s *Service) devDisablePortSKeepalive(conn net.Conn) {
	for {
		counted, ok := conn.(*inboundCountingConn)
		if !ok || counted == nil {
			break
		}
		conn = counted.conn
	}
	disabler, ok := conn.(devKeepaliveDisabler)
	if !ok {
		devLog("IMS experiment could not disable the port-s keepalive", "device", s.DeviceID(),
			"conn_type", fmt.Sprintf("%T", conn))
		return
	}
	err := disabler.DisableKeepAlive()
	devLog("IMS experiment disabled the port-s keepalive", "device", s.DeviceID(),
		"remote", conn.RemoteAddr(), "local", conn.LocalAddr(), "err", err)
}

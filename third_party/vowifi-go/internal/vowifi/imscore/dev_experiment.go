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
	devExperimentNoFeatureCaps, devExperimentRecoveryJitter,
}

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

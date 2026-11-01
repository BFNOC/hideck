package imscore

// [dev] hideck#90: one-variable experiments for Vodafone DE port-s resets.
// HIDECK_DEV90_EXPERIMENT selects at most one change, applied only to the
// vodafone_de_26202 preset; empty or "baseline" keeps today's behaviour.
// Remove before a PR.

import (
	"fmt"
	"net"
	"os"
	"strings"
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
	devExperimentBaseline         = "baseline"
)

// devExperiment returns the active experiment, or "" for the baseline.
func (s *Service) devExperiment() string {
	if s.devCarrierPreset() != devExperimentPreset {
		return ""
	}
	value := strings.ToLower(strings.TrimSpace(os.Getenv(devExperimentEnv)))
	switch value {
	case "", devExperimentBaseline:
		return ""
	case devExperimentNoOptions, devExperimentNoAlias, devExperimentViaPortC, devExperimentViaPortS,
		devExperimentNoOutbound, devExperimentNoPortSKeepalive:
		return value
	}
	s.dev.experimentUnknown.Do(func() {
		devLog("IMS experiment not recognised; running the baseline", "device", s.DeviceID(), "value", value)
	})
	return ""
}

// devExperimentName is the value the [dev] REGISTER context logs.
func (s *Service) devExperimentName() string {
	if experiment := s.devExperiment(); experiment != "" {
		return experiment
	}
	return devExperimentBaseline
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

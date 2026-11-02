package imscore

import (
	"net"
	"strings"
	"testing"
)

func devExperimentRegister(t *testing.T, preset, experiment string) string {
	t.Helper()
	t.Setenv(devExperimentEnv, experiment)
	service := &Service{cfg: &IMSConfig{
		IMPI: "user@ims.example", IMPU: "sip:user@ims.example", Domain: "ims.example",
		LocalIP: net.ParseIP("192.0.2.10"), LocalPort: 5060, Transport: "udp", CarrierPresetID: preset,
	}}
	return service.buildRegister(&registerSession{
		callID: "call-1", fromTag: "tag-1", contactUser: "contact-1", cseq: 2,
		security: &securityAgreement{
			verifyHeader: "ipsec-3gpp;alg=hmac-sha-1-96",
			client:       securityMechanism{PortC: 50309, PortS: 48554},
		},
	}, "Digest response")
}

func TestDevExperimentChangesOnlyItsOwnHeader(t *testing.T) {
	baseline := devExperimentRegister(t, devExperimentPreset, "")
	baseVia, baseSupported := sipHeaderValue(baseline, "Via"), sipHeaderValue(baseline, "Supported")
	if sipViaSentBy(baseVia) != "192.0.2.10:5060" || !strings.Contains(baseVia, ";alias") ||
		!strings.Contains(baseSupported, "outbound") {
		t.Fatalf("baseline Via = %q, Supported = %q", baseVia, baseSupported)
	}
	cases := []struct {
		experiment, sentBy string
		alias, outbound    bool
	}{
		{devExperimentNoAlias, "192.0.2.10:5060", false, true},
		{devExperimentViaPortC, "192.0.2.10:50309", true, true},
		{devExperimentViaPortS, "192.0.2.10:48554", true, true},
		{devExperimentNoOutbound, "192.0.2.10:5060", true, false},
		{devExperimentNoOptions, "192.0.2.10:5060", true, true},
		{devExperimentNoPortSKeepalive, "192.0.2.10:5060", true, true},
		{"not-an-experiment", "192.0.2.10:5060", true, true},
	}
	for _, tc := range cases {
		request := devExperimentRegister(t, devExperimentPreset, tc.experiment)
		via, supported := sipHeaderValue(request, "Via"), sipHeaderValue(request, "Supported")
		if got := sipViaSentBy(via); got != tc.sentBy {
			t.Errorf("%s: Via sent-by = %q, want %q", tc.experiment, got, tc.sentBy)
		}
		if got := strings.Contains(via, ";alias"); got != tc.alias {
			t.Errorf("%s: Via alias = %v, want %v (%q)", tc.experiment, got, tc.alias, via)
		}
		if got := strings.Contains(supported, "outbound"); got != tc.outbound {
			t.Errorf("%s: Supported outbound = %v, want %v (%q)", tc.experiment, got, tc.outbound, supported)
		}
		if want := removeHeaderToken(baseSupported, "outbound"); removeHeaderToken(supported, "outbound") != want {
			t.Errorf("%s: Supported = %q, want only outbound to differ from %q", tc.experiment, supported, baseSupported)
		}
	}
}

func TestDevExperimentIgnoredForOtherPresets(t *testing.T) {
	request := devExperimentRegister(t, "dito_51566", devExperimentViaPortS)
	if got := sipViaSentBy(sipHeaderValue(request, "Via")); got != "192.0.2.10:5060" {
		t.Fatalf("other preset Via sent-by = %q, want unchanged", got)
	}
}

type devKeepaliveConn struct {
	net.Conn
	disabled bool
}

func (c *devKeepaliveConn) DisableKeepAlive() error { c.disabled = true; return nil }
func (c *devKeepaliveConn) RemoteAddr() net.Addr    { return &net.TCPAddr{} }
func (c *devKeepaliveConn) LocalAddr() net.Addr     { return &net.TCPAddr{} }

func TestDevDisablePortSKeepaliveUnwrapsCountingConn(t *testing.T) {
	inner := &devKeepaliveConn{}
	(&Service{cfg: &IMSConfig{}}).devDisablePortSKeepalive(&inboundCountingConn{conn: inner})
	if !inner.disabled {
		t.Fatal("keepalive was not disabled on the wrapped connection")
	}
}

func TestDevExperimentRefreshRouteAndFeatureCaps(t *testing.T) {
	refresh := func(experiment string) string {
		t.Setenv(devExperimentEnv, experiment)
		service := &Service{cfg: &IMSConfig{
			IMPI: "user@ims.example", IMPU: "sip:user@ims.example", Domain: "ims.example",
			LocalIP: net.ParseIP("192.0.2.10"), LocalPort: 5060, Transport: "udp",
			CarrierPresetID: devExperimentPreset,
		}}
		return service.buildRegister(&registerSession{
			callID: "call-1", fromTag: "tag-1", contactUser: "contact-1", cseq: 4,
			path: "<sip:[2001:db8::5]:6000;lr;ottag=ue_term>",
		}, "Digest response")
	}
	cases := []struct {
		experiment         string
		route, featureCaps bool
	}{
		{"", true, true},
		{devExperimentNoRegisterRoute, false, true},
		{devExperimentNoFeatureCaps, true, false},
		{devExperimentNoRegisterRoute + ", " + devExperimentNoFeatureCaps, false, false},
		{devExperimentNoRegisterRoute + ",not-an-experiment", true, true},
	}
	for _, tc := range cases {
		request := refresh(tc.experiment)
		if got := sipHeaderValue(request, "Route") != ""; got != tc.route {
			t.Errorf("%q: Route present = %v, want %v", tc.experiment, got, tc.route)
		}
		if got := sipHeaderValue(request, "Feature-Caps") != ""; got != tc.featureCaps {
			t.Errorf("%q: Feature-Caps present = %v, want %v", tc.experiment, got, tc.featureCaps)
		}
	}
}

func TestDevExperimentNameSortsAndDedupes(t *testing.T) {
	t.Setenv(devExperimentEnv, "recovery-jitter, no-register-route,recovery-jitter,baseline")
	service := &Service{cfg: &IMSConfig{CarrierPresetID: devExperimentPreset}}
	if got := service.devExperimentName(); got != "no-register-route,recovery-jitter" {
		t.Fatalf("experiment name = %q", got)
	}
}

func TestDevRecoveryJitterOnlyAfterPeerReset(t *testing.T) {
	t.Setenv(devExperimentEnv, devExperimentRecoveryJitter)
	service := &Service{cfg: &IMSConfig{CarrierPresetID: devExperimentPreset}}
	if _, ok := service.devRecoveryJitter(); ok {
		t.Fatal("jitter applied without a peer reset")
	}
	service.portSSession.lastCloseKind = portSClosePeerReset
	for range 50 {
		delay, ok := service.devRecoveryJitter()
		if !ok || delay < devRecoveryJitterMin || delay >= devRecoveryJitterMax {
			t.Fatalf("jitter = %v, %v", delay, ok)
		}
	}
}

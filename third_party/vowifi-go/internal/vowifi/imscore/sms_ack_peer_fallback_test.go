package imscore

import (
	"net"
	"strings"
	"testing"

	"github.com/iniwex5/vowifi-go/internal/smscodec"
)

func newRPReportFlowTestService(t *testing.T, portUS bool) (*Service, <-chan string) {
	t.Helper()
	s, _, _ := newInboundSMSTestService(t)
	registered := startProtectedTestTCPOutbound(t, s)
	s.mu.Lock()
	s.cfg.LocalIP = net.IPv4(127, 0, 0, 1)
	s.protectedClientPort, s.protectedServerPort = 5062, 5064
	s.cfg.IMSRegisterTemplate.RPReportOverPortUS = portUS
	s.mu.Unlock()
	return s, registered
}

// brokenPeer stands in for the connection the P-CSCF opened to port_us. Any
// RP report written to it fails, which costs an attempt.
func brokenPeer(t *testing.T) net.Conn {
	t.Helper()
	peer, network := net.Pipe()
	_ = network.Close()
	t.Cleanup(func() { _ = peer.Close() })
	return peer
}

// TS 33.203 7.1: a UE request goes on its own connection from port_uc to port_ps.
func TestRPReportStartsOnRegisteredFlow(t *testing.T) {
	s, registered := newRPReportFlowTestService(t, false)
	raw := inboundSMSRequest(t, imsSMSContentType, inboundRPData(t, 0x46, "+447700900123", "default"))
	attempts, err := s.sendRPReportWithRetryPolicy(rpReportRequest{
		Inbound: raw, Body: smscodec.BuildRPAck(0x46), RPMR: 0x46, PeerConn: brokenPeer(t),
	}, 0, 0)
	if err != nil || attempts != 1 {
		t.Fatalf("attempts=%d error=%v, want one attempt on the registered flow", attempts, err)
	}
	report := waitForOutboundSMSControl(t, registered)
	if body, err := rawSIPBody(report); err != nil || string(body) != string(smscodec.BuildRPAck(0x46)) {
		t.Fatalf("RP report = %x, %v", body, err)
	}
	if !strings.HasPrefix(sipHeaderValue(report, "Via"), "SIP/2.0/TCP ") {
		t.Fatal("RP report did not use the registered TCP flow")
	}
}

func TestRPReportPresetStartsOnPortUSAndFallsBack(t *testing.T) {
	s, registered := newRPReportFlowTestService(t, true)
	raw := inboundSMSRequest(t, imsSMSContentType, inboundRPData(t, 0x44, "+447700900123", "peer"))
	attempts, err := s.sendRPReportWithRetryPolicy(rpReportRequest{
		Inbound: raw, Body: smscodec.BuildRPAck(0x44), RPMR: 0x44, PeerConn: brokenPeer(t),
	}, 0, 0)
	if err != nil || attempts != 2 {
		t.Fatalf("attempts=%d error=%v, want the port_us failure then the registered flow", attempts, err)
	}
	waitForOutboundSMSControl(t, registered)
}

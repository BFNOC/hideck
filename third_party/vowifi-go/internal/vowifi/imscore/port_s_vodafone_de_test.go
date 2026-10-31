package imscore

import (
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

// The wait itself is our choice; TS 33.203 7.1 NOTE 6 only allows it. These
// tests pin the choice: no REGISTER before the cap, one after it, and SMS
// reception not reported ready while nothing shows the P-CSCF delivers.
func TestVodafoneDEPortSResetDefersRecoveryUntilCap(t *testing.T) {
	service := newCarrierAwaitTestService(t, vodafoneDECarrierPresetID)
	closeIdleTestPortS(t, service, syscall.ECONNRESET, 10*time.Second)
	closedAt := service.capturePortSSession().closedAt
	service.handleProtectedServerPushClosed()
	if service.portSRecoveryPending.Load() || service.reRegisterPending.Load() {
		t.Fatal("reset scheduled a REGISTER before the cap")
	}
	if !service.canAwaitOnDemandPortS() {
		t.Fatal("not awaiting right after the reset")
	}
	if got := service.SMSReadiness(); got.Ready {
		t.Fatalf("readiness = %+v, want not ready while awaiting an unproven reopen", got)
	}
	session := service.capturePortSSession()
	if !vodafoneDEPortSResetAwaiting(vodafoneDECarrierPresetID, session, false, closedAt.Add(vodafoneDEPortSResetAwaitCap-time.Second)) ||
		vodafoneDEPortSResetAwaiting(vodafoneDECarrierPresetID, session, false, closedAt.Add(vodafoneDEPortSResetAwaitCap)) {
		t.Fatal("wait does not end at the cap")
	}
	service.portSWatchMu.Lock()
	timer := service.portSWatchTimer
	service.portSWatchMu.Unlock()
	if timer == nil {
		t.Fatal("no recovery watch for the end of the wait")
	}
	stopTestPortSWatch(service)
}

func TestVodafoneDEPortSResetRegistersAfterCap(t *testing.T) {
	service := newCarrierAwaitTestService(t, vodafoneDECarrierPresetID)
	closeIdleTestPortS(t, service, syscall.ECONNRESET, 10*time.Second)
	service.handleProtectedServerPushClosed()
	service.portSSessionMu.Lock()
	service.portSSession.closedAt = time.Now().Add(-vodafoneDEPortSResetAwaitCap - time.Second)
	service.portSSessionMu.Unlock()
	if service.canAwaitOnDemandPortS() {
		t.Fatal("still awaiting after the cap")
	}
	fireTestPortSWatch(t, service)
	if !service.portSRecoveryPending.Load() {
		t.Fatal("cap did not start the port-s recovery REGISTER")
	}
	stopTestPortSWatch(service)
}

func TestVodafoneDEPortSReopenCancelsTheCapWatch(t *testing.T) {
	service := newCarrierAwaitTestService(t, vodafoneDECarrierPresetID)
	closeIdleTestPortS(t, service, syscall.ECONNRESET, 10*time.Second)
	service.handleProtectedServerPushClosed()
	reopened, peer := net.Pipe()
	t.Cleanup(func() { reopened.Close(); peer.Close() })
	if !service.trackProtectedPortSConnection(reopened, time.Now()) {
		t.Fatal("track reopened port-s")
	}
	if timer := stopTestPortSWatch(service); timer != nil {
		t.Fatal("peer reopen left the cap watch armed")
	}
	if got := service.SMSReadiness(); !got.Ready {
		t.Fatalf("readiness = %+v, want ready once the P-CSCF reopened port-s", got)
	}
}

func TestVodafoneDEPortSOutboundFlowKeepsRecovery(t *testing.T) {
	service := newCarrierAwaitTestService(t, vodafoneDECarrierPresetID)
	service.sipOutboundFlow.Store(true)
	closeIdleTestPortS(t, service, syscall.ECONNRESET, 10*time.Second)
	if service.canAwaitOnDemandPortS() {
		t.Fatal("awaited a reset on a negotiated outbound flow (TS 24.229 4.2A NOTE 2)")
	}
}

func TestVodafoneDEPortSEOFKeepsRecovery(t *testing.T) {
	service := newCarrierAwaitTestService(t, vodafoneDECarrierPresetID)
	closeIdleTestPortS(t, service, io.EOF, 10*time.Second)
	if service.canAwaitOnDemandPortS() {
		t.Fatal("a clean EOF on Vodafone DE has no on-demand evidence")
	}
}

func TestPortSResetAwaitCapIsVodafoneDEOnly(t *testing.T) {
	session := portSSessionSnapshot{lastCloseKind: portSClosePeerReset, closedAt: time.Now()}
	for _, preset := range []string{"", vodafoneUKCarrierPresetID, ditoPHCarrierPresetID, oneNZCarrierPresetID} {
		if vodafoneDEPortSResetAwaiting(preset, session, false, time.Now()) {
			t.Fatalf("preset %q took the Vodafone DE wait", preset)
		}
	}
}

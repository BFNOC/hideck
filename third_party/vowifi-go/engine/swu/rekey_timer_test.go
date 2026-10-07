package swu

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/engine/ikev2"
)

func TestRekeyTimerRetriesOnceThenSurfacesFailure(t *testing.T) {
	session := NewSession(&Config{})
	sentinel := errors.New("transient rekey failure")
	attempts := 0
	session.startRekeyTimer(rekeyTimerSpec{
		name: "test", interval: time.Millisecond, target: &session.ikeRekeyTimer,
		retryInterval: time.Millisecond, action: func() error {
			attempts++
			return sentinel
		},
	})
	waitForRekeyTimerFailure(t, session)
	if attempts != rekeyMaxFailures {
		t.Fatalf("attempts = %d, want %d", attempts, rekeyMaxFailures)
	}
	if !errors.Is(session.TerminalError(), sentinel) {
		t.Fatalf("terminal error = %v", session.TerminalError())
	}
}

func TestChildSANotFoundFailsTimerWithoutRetry(t *testing.T) {
	session := NewSession(&Config{})
	attempts := 0
	session.startRekeyTimer(rekeyTimerSpec{
		name: "CHILD_SA", interval: time.Millisecond, target: &session.childRekeyTimer,
		retryInterval: time.Millisecond, immediateFail: isChildSANotFoundError,
		action: func() error {
			attempts++
			return &createChildSARejectError{NotifyType: ikev2.CHILD_SA_NOT_FOUND}
		},
	})
	waitForRekeyTimerFailure(t, session)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestNoAdditionalSAsKeepsSessionAndAsksAgainNextInterval(t *testing.T) {
	session := NewSession(&Config{})
	var attempts atomic.Int32
	session.startRekeyTimer(rekeyTimerSpec{
		name: "CHILD_SA", interval: time.Millisecond, target: &session.childRekeyTimer,
		retryInterval: time.Hour, declined: isNoAdditionalSAsError,
		action: func() error {
			attempts.Add(1)
			return &createChildSARejectError{NotifyType: ikev2.NO_ADDITIONAL_SAS}
		},
	})
	deadline := time.After(time.Second)
	for attempts.Load() < rekeyMaxFailures+1 {
		select {
		case <-session.done:
			t.Fatalf("declined rekey failed the session: %v", session.TerminalError())
		case <-deadline:
			t.Fatalf("attempts = %d, want the timer re-armed after each decline", attempts.Load())
		case <-time.After(time.Millisecond):
		}
	}
	session.cancel()
	session.rekeyTimerWG.Wait()
	if err := session.TerminalError(); err != nil {
		t.Fatalf("terminal error = %v, want session kept", err)
	}
}

// A replacement refused after CHILD_SA_NOT_FOUND leaves no Child SA at all,
// so it must fail the session instead of being treated as a decline.
func TestDeclinedReplacementAfterChildSANotFoundFailsSession(t *testing.T) {
	session := NewSession(&Config{})
	var attempts atomic.Int32
	session.startRekeyTimer(rekeyTimerSpec{
		name: "CHILD_SA", interval: time.Millisecond, target: &session.childRekeyTimer,
		retryInterval: time.Millisecond,
		immediateFail: isChildSANotFoundError, declined: isNoAdditionalSAsError,
		action: func() error {
			attempts.Add(1)
			return errors.Join(
				&createChildSARejectError{NotifyType: ikev2.CHILD_SA_NOT_FOUND},
				&createChildSARejectError{NotifyType: ikev2.NO_ADDITIONAL_SAS},
			)
		},
	})
	waitForRekeyTimerFailure(t, session)
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func waitForRekeyTimerFailure(t *testing.T, session *Session) {
	t.Helper()
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for rekey timer failure")
	}
	session.rekeyTimerWG.Wait()
}

func TestIKERekeyAnswerWithoutNewSAIsDeclined(t *testing.T) {
	_, err := (&Session{}).validateIKESARekeyResponse(nil)
	if !errors.Is(err, errIKERekeyNoNewSA) || !strings.Contains(err.Error(), "payloads: none") {
		t.Fatalf("validate error = %v", err)
	}
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("wrapped: %w", err), true},
		{&IKEAuthError{NotifyType: ikev2.NO_ADDITIONAL_SAS}, true},
		{&IKEAuthError{NotifyType: ikev2.AUTHENTICATION_FAILED}, false},
		{errors.New("timeout reached max retries"), false},
	}
	for _, c := range cases {
		if got := isIKERekeyDeclined(c.err); got != c.want {
			t.Fatalf("isIKERekeyDeclined(%v) = %v, want %v", c.err, got, c.want)
		}
	}
	partial := []ikev2.Payload{
		&ikev2.EncryptedPayloadSA{Proposals: []*ikev2.Proposal{{}, {}}},
		&ikev2.EncryptedPayloadNonce{Data: []byte{1}},
	}
	_, err = (&Session{}).validateIKESARekeyResponse(partial)
	if err == nil || isIKERekeyDeclined(err) || !strings.Contains(err.Error(), "SA(2 proposals)") {
		t.Fatalf("partial answer error = %v, want a non-declined failure naming the payloads", err)
	}
}

func TestDeclinedIKESARekeyLeavesSessionUnchanged(t *testing.T) {
	session, transport := newEstablishedControlSession(t)
	defer stopControlTestSession(session)
	session.mu.RLock()
	oldSPIi, oldSPIr, oldKeys, oldID := session.spiI, session.spiR, session.ikeKeys, session.nextOutboundID
	session.mu.RUnlock()
	go func() {
		request, err := ikev2.DecodePacket(<-transport.sentIKE)
		if err != nil {
			t.Errorf("decode IKE rekey request: %v", err)
			return
		}
		encoded, _ := session.encryptAndWrap(&ikev2.IKEPacket{
			InitiatorSPI: request.InitiatorSPI, ResponderSPI: request.ResponderSPI,
			Version: 0x20, ExchangeType: request.ExchangeType,
			Flags: ikeResponseFlag, MessageID: request.MessageID,
		})
		transport.ike <- encoded
	}()
	err := session.RekeyIKESA()
	if !isIKERekeyDeclined(err) {
		t.Fatalf("RekeyIKESA error = %v, want a decline", err)
	}
	session.mu.RLock()
	defer session.mu.RUnlock()
	if session.spiI != oldSPIi || session.spiR != oldSPIr || session.ikeKeys != oldKeys ||
		allZero(oldKeys.SK_d) || session.retiredIKESA != nil || session.nextOutboundID != oldID+1 {
		t.Fatalf("declined rekey changed the IKE SA: nextOutboundID %d -> %d, retired=%v",
			oldID, session.nextOutboundID, session.retiredIKESA != nil)
	}
}

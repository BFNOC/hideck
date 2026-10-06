package swu

import (
	"errors"
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

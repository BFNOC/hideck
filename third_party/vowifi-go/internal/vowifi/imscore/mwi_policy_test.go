package imscore

import (
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

type fakeMWIPolicyStore struct {
	mode     string
	rejected int
	loadErr  error
	saved    []int
	identity string
	iccid    string
}

func (f *fakeMWIPolicyStore) LoadMWISubscriptionPolicy(iccid, identity string) (string, int, error) {
	return f.mode, f.rejected, f.loadErr
}

func (f *fakeMWIPolicyStore) SaveMWISubscriptionRejection(iccid, identity string, status int) error {
	f.saved, f.iccid, f.identity = append(f.saved, status), iccid, identity
	return nil
}

func newMWIPolicyTestService(t *testing.T, store *fakeMWIPolicyStore, carrierDisabled bool) *Service {
	t.Helper()
	s := newSubscriptionLifecycleTestService(t, nil)
	s.cfg.ICCID = "8949020000000000001"
	s.cfg.IMSRegisterTemplate.DisableMWISubscription = carrierDisabled
	if store != nil {
		s.cfg.MWIPolicyStore = store
	}
	return s
}

func TestMWISubscriptionBlockedReason(t *testing.T) {
	cases := []struct {
		name            string
		store           *fakeMWIPolicyStore
		carrierDisabled bool
		blocked         bool
	}{
		{name: "no store follows carrier default", blocked: false},
		{name: "no store follows carrier opt-out", carrierDisabled: true, blocked: true},
		{name: "auto follows carrier opt-out", store: &fakeMWIPolicyStore{mode: MWISubscriptionAuto}, carrierDisabled: true, blocked: true},
		{name: "auto subscribes elsewhere", store: &fakeMWIPolicyStore{mode: MWISubscriptionAuto}, blocked: false},
		{name: "on overrides carrier opt-out", store: &fakeMWIPolicyStore{mode: MWISubscriptionOn}, carrierDisabled: true, blocked: false},
		{name: "off never subscribes", store: &fakeMWIPolicyStore{mode: MWISubscriptionOff}, blocked: true},
		{name: "remembered rejection blocks auto", store: &fakeMWIPolicyStore{mode: MWISubscriptionAuto, rejected: 405}, blocked: true},
		{name: "remembered rejection blocks on", store: &fakeMWIPolicyStore{mode: MWISubscriptionOn, rejected: 489}, blocked: true},
		{name: "unreadable policy does not subscribe", store: &fakeMWIPolicyStore{loadErr: errors.New("db closed")}, blocked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newMWIPolicyTestService(t, tc.store, tc.carrierDisabled)
			if reason := s.mwiSubscriptionBlockedReason(); (reason != "") != tc.blocked {
				t.Fatalf("blocked reason = %q, want blocked=%v", reason, tc.blocked)
			}
		})
	}
}

func TestMWISubscriptionOffDoesNotStart(t *testing.T) {
	s := newMWIPolicyTestService(t, &fakeMWIPolicyStore{mode: MWISubscriptionOff}, false)
	s.startMWISubscription()
	s.mu.RLock()
	started := s.mwiSubscriptionLifecycle.started
	s.mu.RUnlock()
	if started || s.mwiSubscriptionInFlight.Load() {
		t.Fatal("SIM setting off still started SUBSCRIBE(mwi)")
	}
}

func TestBlockedMWIRefreshStopsTheUsage(t *testing.T) {
	store := &fakeMWIPolicyStore{mode: MWISubscriptionAuto}
	s := newMWIPolicyTestService(t, store, false)
	if start, reason := s.prepareSubscriptionStart(true); !start {
		t.Fatal(reason)
	}
	s.mu.Lock()
	s.mwiSubscriptionDialog = registrationSubscriptionDialog{callID: "call", localTag: "local", remoteTag: "remote"}
	s.mwiSubscriptionRefreshAt = time.Now()
	s.mu.Unlock()
	store.mode = MWISubscriptionOff
	s.refreshMWISubscription()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.mwiSubscriptionClosed || !s.mwiSubscriptionRefreshAt.IsZero() || s.mwiSubscriptionDialog.ready() {
		t.Fatal("blocked refresh left the MWI usage scheduled")
	}
	if s.mwiSubscriptionInFlight.Load() {
		t.Fatal("blocked refresh still sent SUBSCRIBE(mwi)")
	}
}

func TestNetworkMWIRejectionIsRemembered(t *testing.T) {
	for _, status := range []int{405, 489} {
		store := &fakeMWIPolicyStore{mode: MWISubscriptionAuto}
		s := newMWIPolicyTestService(t, store, false)
		recordMWIStatusForTest(t, s, status, false)
		if len(store.saved) != 1 || store.saved[0] != status {
			t.Fatalf("status %d saved = %v", status, store.saved)
		}
		if store.iccid != s.cfg.ICCID || store.identity == "" {
			t.Fatalf("status %d saved for iccid %q identity %q", status, store.iccid, store.identity)
		}
	}
}

func TestTemporaryOrLocalMWIFailuresAreNotRemembered(t *testing.T) {
	for _, tc := range []struct {
		status     int
		selfRouted bool
	}{{480, false}, {408, false}, {500, false}, {403, false}, {405, true}} {
		store := &fakeMWIPolicyStore{mode: MWISubscriptionAuto}
		s := newMWIPolicyTestService(t, store, false)
		recordMWIStatusForTest(t, s, tc.status, tc.selfRouted)
		if len(store.saved) != 0 {
			t.Fatalf("status %d self-routed=%v was remembered", tc.status, tc.selfRouted)
		}
	}
}

func recordMWIStatusForTest(t *testing.T, s *Service, status int, selfRouted bool) {
	t.Helper()
	attempt, err := s.beginSubscriptionAttempt(true, false)
	if err != nil {
		t.Fatal(err)
	}
	if selfRouted {
		s.mu.Lock()
		s.mwiSubscriptionLifecycle.selfRouted = true
		s.mu.Unlock()
	}
	result := subscriptionResult{context: attempt, response: &sip.Response{StatusCode: status}, err: errors.New("subscription failed")}
	if err := s.recordMWISubscriptionResult(result); err == nil {
		t.Fatal("failure was hidden")
	}
}

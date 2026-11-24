package device

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/esim"
	"github.com/yibaiba/hideck/internal/vowifihost"
	"github.com/yibaiba/hideck/pkg/logger"
)

const supersededRestoreDeadline = 5 * time.Second

func waitSupersededRestoreSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(supersededRestoreDeadline):
		t.Fatalf("%s within %s", what, supersededRestoreDeadline)
	}
}

// Field log 2026-11-23 01:22:06: switch 4 began while switch 3's restore was
// still running, and switch 3's canceled restore marked switch 4 degraded.
func TestPostSwitchRestoreSupersededByNewerSwitchLeavesNewerSwitchAlone(t *testing.T) {
	setupRestoreWaitLogger(t)
	const deviceID = "dev-restore-superseded"
	p := NewPool(&config.Config{})
	be := &esimSwitchRestoreBackendStub{
		mode:               backend.BackendQMI,
		getMode:            backend.ModeOnline,
		liveICCID:          "460001234567890",
		liveIMSI:           "460001234567890",
		resolvedSIMAuthAID: "A0000000871002FFFFFFFF8903020000",
		resolvedSIMAuthSrc: "qmi_card_status",
	}
	p.workers[deviceID] = &Worker{
		ID:      deviceID,
		Config:  config.DeviceConfig{ID: deviceID, VoWiFiEnabled: true},
		Backend: be,
	}
	retryDelays := postSwitchIdentityRetryDelays
	postSwitchIdentityRetryDelays = nil
	t.Cleanup(func() { postSwitchIdentityRetryDelays = retryDelays })
	t.Cleanup(func() {
		p.voWiFiHost().InvalidateRuntime(deviceID, "test_cleanup")
		p.cancel()
		p.clearESIMSwitch(deviceID)
	})
	restoreStarted := make(chan struct{})
	var once sync.Once
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		if cmd.Kind != vowifihost.LifecycleCommandSwitchEnd {
			return nil
		}
		once.Do(func() { close(restoreStarted) })
		<-ctx.Done()
		return ctx.Err()
	}

	older := p.handleESIMSwitchBefore(deviceID, "")
	olderDone := make(chan struct{})
	go func() {
		p.handleESIMSwitchAfter(deviceID, older)
		close(olderDone)
	}()
	waitSupersededRestoreSignal(t, restoreStarted, "older switch did not submit switch_end")

	logs := logger.GlobalBroadcaster.Subscribe()
	defer logger.GlobalBroadcaster.Unsubscribe(logs)
	newerToken := make(chan uint64, 1)
	go func() { newerToken <- p.handleESIMSwitchBefore(deviceID, "") }()
	var newer uint64
	select {
	case newer = <-newerToken:
	case <-time.After(supersededRestoreDeadline):
		t.Fatalf("newer switch did not begin within %s", supersededRestoreDeadline)
	}
	waitSupersededRestoreSignal(t, olderDone, "older post-switch flow did not return after the newer switch began")

	got, ok := currentSwitchSnapshotSwitchRestore(t, p, deviceID)
	if !ok || got.SwitchToken != newer || newer == older {
		t.Fatalf("current switch ok=%v token=%d, want newer token %d (older %d)", ok, got.SwitchToken, newer, older)
	}
	if got.Phase != esim.SwitchPhasePrepare {
		t.Fatalf("newer switch phase=%q, want %q untouched by the older restore", got.Phase, esim.SwitchPhasePrepare)
	}
	if len(be.setCalls) != 0 {
		t.Fatalf("setCalls=%v, want no radio/data fallback from the older snapshot", be.setCalls)
	}
	entry := waitLogEntry(t, logs, func(entry logger.LogEntry) bool {
		return entry.Message == "["+deviceID+"] "+postSwitchRestoreLeftMsg
	})
	if fields := readLogFields(t, entry); fields["switch_token"] != float64(older) || fields["reason"] != "newer_switch" {
		t.Fatalf("superseded restore log fields=%v, want switch_token=%d reason=newer_switch", fields, older)
	}
}

func TestPostSwitchRestoreGateFailureDoesNotMarkNewerSwitchDegraded(t *testing.T) {
	const deviceID = "dev-restore-gate-superseded"
	p := NewPool(&config.Config{})
	t.Cleanup(func() {
		p.cancel()
		p.clearESIMSwitch(deviceID)
	})
	be := &esimSwitchRestoreBackendStub{mode: backend.BackendQMI, getMode: backend.ModeOnline}
	w := &Worker{
		ID:      deviceID,
		Config:  config.DeviceConfig{ID: deviceID, VoWiFiEnabled: true},
		Backend: be,
	}
	p.workers[deviceID] = w
	older := p.beginESIMSwitch(deviceID, "")
	newer := p.beginESIMSwitch(deviceID, "")

	p.restorePostSwitchConnectivity(deviceID, w, older, errors.New("simauth gate not ready"), true)

	got, ok := currentSwitchSnapshotSwitchRestore(t, p, deviceID)
	if !ok || got.SwitchToken != newer.SwitchToken {
		t.Fatalf("current switch ok=%v token=%d, want %d", ok, got.SwitchToken, newer.SwitchToken)
	}
	if got.Phase != esim.SwitchPhasePrepare {
		t.Fatalf("newer switch phase=%q, want %q untouched by the older gate failure", got.Phase, esim.SwitchPhasePrepare)
	}
	if len(be.setCalls) != 0 {
		t.Fatalf("setCalls=%v, want no radio/data fallback from the older snapshot", be.setCalls)
	}
}

// The snapshot radio/data fallback runs as in main for the last switch begun on
// the device, also after the watchdog cleared its record, and never for an older
// switch, whether the newer one is still running or already finished.
func TestPostSwitchRadioFallbackOnlyForCurrentSwitch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lebara  bool
		vowifi  bool
		gateErr error
	}{
		{name: "lebara-recover-lock", lebara: true, vowifi: true},
		{name: "vowifi-disabled"},
		{name: "gate-failure", vowifi: true, gateErr: errors.New("simauth gate not ready")},
	} {
		for _, state := range []string{"current", "watchdog", "newer-running", "newer-finished"} {
			deviceID := "dev-fallback-" + tc.name + "-" + state
			t.Run(deviceID, func(t *testing.T) {
				logs := collectRestoreWaitLogs(t, deviceID)
				p := NewPool(&config.Config{})
				t.Cleanup(func() {
					p.cancel()
					p.clearESIMSwitch(deviceID)
				})
				be := &esimSwitchRestoreBackendStub{
					mode:      backend.BackendQMI,
					getMode:   backend.ModeOnline,
					liveICCID: "460001234567890",
					liveIMSI:  "460001234567890",
				}
				w := &Worker{
					ID:      deviceID,
					Config:  config.DeviceConfig{ID: deviceID, VoWiFiEnabled: tc.vowifi},
					Backend: be,
				}
				p.workers[deviceID] = w
				if tc.lebara && !p.beginLebaraUKIdentityRecover(w, "460001234567890", false) {
					t.Fatal("begin Lebara recover lock")
				}
				older := p.beginESIMSwitch(deviceID, "")
				switch state {
				case "watchdog":
					clearSwitchRecordLikeWatchdog(p, deviceID)
				case "newer-running":
					p.beginESIMSwitch(deviceID, "")
				case "newer-finished":
					p.clearESIMSwitchIfToken(deviceID, p.beginESIMSwitch(deviceID, "").SwitchToken)
				}

				p.restorePostSwitchConnectivity(deviceID, w, older, tc.gateErr, true)

				if state == "current" || state == "watchdog" {
					if want := []backend.OperatingMode{backend.ModeOnline}; !reflect.DeepEqual(be.setCalls, want) {
						t.Fatalf("setCalls=%v, want %v from the radio/data fallback", be.setCalls, want)
					}
					if len(logs.fields(t, postSwitchRestoreLeftMsg)) != 0 {
						t.Fatal("restore of the last switch was treated as superseded")
					}
					return
				}
				if len(be.setCalls) != 0 {
					t.Fatalf("setCalls=%v, want no radio/data fallback from the older snapshot", be.setCalls)
				}
				if fields := logs.waitFields(t, postSwitchRestoreLeftMsg, nil); fields["switch_token"] != float64(older.SwitchToken) ||
					fields["reason"] != "newer_switch" {
					t.Fatalf("left restore log fields=%v, want switch_token=%d reason=newer_switch", fields, older.SwitchToken)
				}
			})
		}
	}
}

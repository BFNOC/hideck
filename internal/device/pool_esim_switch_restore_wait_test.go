package device

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/esim"
	"github.com/yibaiba/hideck/internal/vowifihost"
	"github.com/yibaiba/hideck/pkg/logger"
)

const (
	restoreWaitSwitchToken = 1
	restoreWaitDeadline    = 5 * time.Second
	restoreWaitPhaseMsg    = "eSIM 切卡阶段更新"
	restoreWaitExceededMsg = "切卡后 VoWiFi 恢复超过等待上限，结束切卡，恢复转入后台继续"
	restoreWaitCanceledMsg = "切卡后后台 VoWiFi 恢复已被其他操作取消，交给该操作处理，不标记降级、不做射频/数据兜底"
	restoreWaitFailedMsg   = "切卡后恢复 VoWiFi 失败"
	restoreWaitBgDoneMsg   = "切卡后后台 VoWiFi 恢复完成"
	restoreWaitBgFailedMsg = "切卡后后台 VoWiFi 恢复失败"
	restoreWaitNotOwnedMsg = "切卡后恢复返回时设备已有新的切卡、worker 已替换或服务停止，跳过 eSIM 总览预热和收尾"
)

func withPostSwitchVoWiFiRestoreWait(t *testing.T, wait time.Duration) {
	t.Helper()
	original := postSwitchVoWiFiRestoreWait
	postSwitchVoWiFiRestoreWait = wait
	t.Cleanup(func() { postSwitchVoWiFiRestoreWait = original })
}

// newRestoreWaitSwitch sets up a device whose post-switch flow reaches SwitchEnd,
// the same way TestHandleESIMSwitchAfterSubmitsSwitchEndThroughLifecycleController does.
func newRestoreWaitSwitch(t *testing.T, deviceID string) (*Pool, *esimSwitchRestoreBackendStub) {
	t.Helper()
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
	withSwitchSnapshot(p, deviceID, esimSwitchContext{SwitchToken: restoreWaitSwitchToken})
	p.switchMu.Lock()
	p.switchTokens[deviceID] = restoreWaitSwitchToken
	p.latestSwitchTokens[deviceID] = restoreWaitSwitchToken
	p.switchSeq = restoreWaitSwitchToken
	p.switchMu.Unlock()
	// The delayed identity refreshes would outlive the test and log into the
	// next one's logger.
	retryDelays := postSwitchIdentityRetryDelays
	postSwitchIdentityRetryDelays = nil
	t.Cleanup(func() { postSwitchIdentityRetryDelays = retryDelays })
	t.Cleanup(func() {
		// Stopping the pool first sends a restore still running down the
		// pool_stopped exit, so teardown starts no radio/data fallback.
		p.cancel()
		// Releases a restore run that a regression left blocking the switch.
		p.voWiFiHost().InvalidateRuntime(deviceID, "test_cleanup")
		// A switch record left behind would let the two minute watchdog log
		// into a later test's logger.
		p.clearESIMSwitch(deviceID)
	})
	return p, be
}

// clearSwitchRecordLikeWatchdog deletes the switch record the way the two
// minute switch watchdog does. It leaves the latest switch token alone.
func clearSwitchRecordLikeWatchdog(p *Pool, deviceID string) {
	p.switchMu.Lock()
	delete(p.switchContexts, deviceID)
	delete(p.switchingDevices, deviceID)
	delete(p.switchTokens, deviceID)
	p.switchMu.Unlock()
}

// watchModes sends every operating mode set on be, for a fallback that runs on
// a goroutine the test does not wait for.
func watchModes(be *esimSwitchRestoreBackendStub) <-chan backend.OperatingMode {
	modes := make(chan backend.OperatingMode, 16)
	be.setModeHook = func(mode backend.OperatingMode) { modes <- mode }
	return modes
}

func requireMode(t *testing.T, modes <-chan backend.OperatingMode, want backend.OperatingMode) {
	t.Helper()
	select {
	case got := <-modes:
		if got != want {
			t.Fatalf("operating mode=%v, want %v from the snapshot fallback", got, want)
		}
	case <-time.After(restoreWaitDeadline):
		t.Fatalf("no operating mode set within %s, want %v from the snapshot fallback", restoreWaitDeadline, want)
	}
}

func requireNoMode(t *testing.T, modes <-chan backend.OperatingMode) {
	t.Helper()
	select {
	case mode := <-modes:
		t.Fatalf("operating mode %v set, want none", mode)
	default:
	}
}

// waitBackgroundRestores waits until every restore left running has handled
// its result, fallback included.
func waitBackgroundRestores(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		postSwitchBackgroundRestores.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(restoreWaitDeadline):
		t.Errorf("a restore left running did not finish within %s", restoreWaitDeadline)
	}
}

func stageIs(stage string) func(map[string]any) bool {
	return func(fields map[string]any) bool { return fields["stage"] == stage }
}

// blockSwitchEndUntilCanceled makes the restored VoWiFi start hang like an ePDG
// that keeps rejecting: the run returns only when the lifecycle controller cancels it.
func blockSwitchEndUntilCanceled(p *Pool) (<-chan struct{}, <-chan error) {
	started := make(chan struct{})
	stopped := make(chan error, 1)
	var once sync.Once
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		if cmd.Kind != vowifihost.LifecycleCommandSwitchEnd {
			return nil
		}
		once.Do(func() { close(started) })
		<-ctx.Done()
		select {
		case stopped <- ctx.Err():
		default:
		}
		return ctx.Err()
	}
	return started, stopped
}

func finishSwitchWithin(t *testing.T, p *Pool, deviceID string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		p.handleESIMSwitchAfter(deviceID, restoreWaitSwitchToken)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(restoreWaitDeadline):
		t.Fatalf("handleESIMSwitchAfter still running after %s, IsESIMSwitching=%v",
			restoreWaitDeadline, p.IsESIMSwitching(deviceID))
	}
}

type restoreWaitLogs struct {
	prefix  string
	mu      sync.Mutex
	entries []logger.LogEntry
}

// setupRestoreWaitLogger routes the global logger to the log stream for one
// test and puts the previous logger back afterwards.
func setupRestoreWaitLogger(t *testing.T) {
	t.Helper()
	prevLog, prevSugar := logger.ZapLogger(), logger.SugarLogger()
	logger.Setup(logger.LogConfig{Debug: true, Filename: filepath.Join(t.TempDir(), "app.log")})
	t.Cleanup(func() {
		// Every goroutine the test started has logged its last line by now.
		logger.Log, logger.Sugar = prevLog, prevSugar
	})
}

// collectRestoreWaitLogs drains the log stream for deviceID while the test runs,
// so a busy post-switch flow cannot overflow the subscriber buffer. Call it
// before the pool is set up, so the pool cleanup runs first.
func collectRestoreWaitLogs(t *testing.T, deviceID string) *restoreWaitLogs {
	t.Helper()
	setupRestoreWaitLogger(t)
	logs := &restoreWaitLogs{prefix: "[" + deviceID + "] "}
	ch := logger.GlobalBroadcaster.Subscribe()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for entry := range ch {
			if !strings.HasPrefix(entry.Message, logs.prefix) {
				continue
			}
			logs.mu.Lock()
			logs.entries = append(logs.entries, entry)
			logs.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		// The pool cleanup has canceled any restore still running; wait for its
		// goroutine to finish before the logger is put back.
		waitBackgroundRestores(t)
		logger.GlobalBroadcaster.Unsubscribe(ch)
		<-drained
	})
	return logs
}

func (l *restoreWaitLogs) fields(t *testing.T, msg string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	entries := slices.Clone(l.entries)
	l.mu.Unlock()
	var out []map[string]any
	for _, entry := range entries {
		if entry.Message == l.prefix+msg {
			out = append(out, readLogFields(t, entry))
		}
	}
	return out
}

// level returns the level of the first entry logged with msg.
func (l *restoreWaitLogs) level(msg string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry.Message == l.prefix+msg {
			return entry.Level
		}
	}
	return ""
}

func (l *restoreWaitLogs) waitFields(t *testing.T, msg string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(restoreWaitDeadline)
	for {
		for _, fields := range l.fields(t, msg) {
			if match == nil || match(fields) {
				return fields
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("log %q not found", l.prefix+msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// phasesAfterDone waits for the switch to log phase done; every earlier phase
// update came from the same goroutine, so it has been collected by then.
func (l *restoreWaitLogs) phasesAfterDone(t *testing.T) []string {
	t.Helper()
	l.waitFields(t, restoreWaitPhaseMsg, func(fields map[string]any) bool {
		return fields["switch_phase"] == string(esim.SwitchPhaseDone)
	})
	var phases []string
	for _, fields := range l.fields(t, restoreWaitPhaseMsg) {
		phase, _ := fields["switch_phase"].(string)
		phases = append(phases, phase)
	}
	return phases
}

func TestHandleESIMSwitchAfterFinishesSwitchWhenVoWiFiRestoreOutlastsWait(t *testing.T) {
	withPostSwitchVoWiFiRestoreWait(t, 50*time.Millisecond)
	const deviceID = "dev-restore-wait-blocked"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	started, stopped := blockSwitchEndUntilCanceled(p)

	finishSwitchWithin(t, p, deviceID)

	waitSupersededRestoreSignal(t, started, "post-switch SwitchEnd was not submitted")
	if p.IsESIMSwitching(deviceID) {
		t.Fatal("device still switching after the restore wait elapsed")
	}
	phases := logs.phasesAfterDone(t)
	if !slices.Contains(phases, string(esim.SwitchPhaseVoWiFiRestore)) {
		t.Fatalf("phases=%v, want %s before done", phases, esim.SwitchPhaseVoWiFiRestore)
	}
	if last := phases[len(phases)-1]; last != string(esim.SwitchPhaseDone) {
		t.Fatalf("last phase=%s phases=%v, want done", last, phases)
	}
	if slices.Contains(phases, string(esim.SwitchPhaseDegraded)) {
		t.Fatalf("phases=%v, want no degraded while the restore keeps running", phases)
	}
	if len(be.setCalls) != 0 {
		t.Fatalf("setCalls=%v, want no radio/data fallback while the restore keeps running", be.setCalls)
	}
	if len(logs.fields(t, restoreWaitFailedMsg)) != 0 {
		t.Fatal("restore failure logged although the restore is still running")
	}
	select {
	case err := <-stopped:
		t.Fatalf("restore run ended with %v, want it left running in the background", err)
	default:
	}
	exceeded := logs.waitFields(t, restoreWaitExceededMsg, nil)
	waited, _ := exceeded["waited_ms"].(float64)
	if exceeded["switch_token"] != float64(restoreWaitSwitchToken) || exceeded["limit_ms"] != float64(50) || waited < 50 {
		t.Fatalf("wait exceeded log fields=%v, want switch_token=%d limit_ms=50 waited_ms>=50", exceeded, restoreWaitSwitchToken)
	}
}

// Disable can cancel a restore left running once the switch is over. The
// disable then owns the radio/data and projects the card policy itself, so the
// canceled restore marks nothing degraded and starts no snapshot fallback.
func TestDisableVoWiFiCancelsPostSwitchRestoreLeftRunning(t *testing.T) {
	withPostSwitchVoWiFiRestoreWait(t, 50*time.Millisecond)
	const deviceID = "dev-restore-wait-disable"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, _ := newRestoreWaitSwitch(t, deviceID)
	_, stopped := blockSwitchEndUntilCanceled(p)

	finishSwitchWithin(t, p, deviceID)
	phasesAtDone := logs.phasesAfterDone(t)
	if last := phasesAtDone[len(phasesAtDone)-1]; last != string(esim.SwitchPhaseDone) {
		t.Fatalf("last phase=%s phases=%v, want done", last, phasesAtDone)
	}

	disabled := make(chan error, 1)
	go func() { disabled <- p.DisableVoWiFi(deviceID) }()
	select {
	case err := <-disabled:
		if err != nil {
			t.Fatalf("DisableVoWiFi() error = %v", err)
		}
	case <-time.After(restoreWaitDeadline):
		t.Fatal("DisableVoWiFi did not return")
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("restore run ctx err=%v, want context.Canceled", err)
		}
	case <-time.After(restoreWaitDeadline):
		t.Fatal("DisableVoWiFi did not cancel the restore left running")
	}
	waitBackgroundRestores(t)
	assertBackgroundRestoreLog(t, logs.waitFields(t, restoreWaitCanceledMsg, nil))
	if len(logs.fields(t, restoreWaitFailedMsg)) != 0 || len(logs.fields(t, postSwitchRestoreLeftMsg)) != 0 {
		t.Fatal("restore canceled by the disable went on to the degraded mark or the fallback")
	}
	if phases := logs.fields(t, restoreWaitPhaseMsg); len(phases) != len(phasesAtDone) {
		t.Fatalf("phase updates after done: %v", phases[len(phasesAtDone):])
	}
}

// Whatever cancels a restore left running owns the device, a Lebara card or
// not: the restore sets no operating mode after the cancel.
func TestBackgroundPostSwitchRestoreCanceledLeavesTheRadioToTheCanceler(t *testing.T) {
	for _, lebara := range []bool{false, true} {
		name := "plain"
		if lebara {
			name = "lebara"
		}
		t.Run(name, func(t *testing.T) {
			withPostSwitchVoWiFiRestoreWait(t, 50*time.Millisecond)
			deviceID := "dev-restore-wait-canceled-" + name
			logs := collectRestoreWaitLogs(t, deviceID)
			var p *Pool
			var modes <-chan backend.OperatingMode
			if lebara {
				p, _, modes = lebaraRestoreWaitSwitch(t, deviceID)
			} else {
				var be *esimSwitchRestoreBackendStub
				p, be = newRestoreWaitSwitch(t, deviceID)
				modes = watchModes(be)
			}
			_, stopped := blockSwitchEndUntilCanceled(p)
			finishSwitchWithin(t, p, deviceID)
			requireNoMode(t, modes)

			p.voWiFiHost().InvalidateRuntime(deviceID, "test_cancel")
			select {
			case <-stopped:
			case <-time.After(restoreWaitDeadline):
				t.Fatal("the restore left running was not canceled")
			}
			waitBackgroundRestores(t)

			requireNoMode(t, modes)
			assertBackgroundRestoreLog(t, logs.waitFields(t, restoreWaitCanceledMsg, nil))
			if len(logs.fields(t, restoreWaitFailedMsg)) != 0 {
				t.Fatal("canceled background restore went on to the fallback")
			}
		})
	}
}

func TestHandleESIMSwitchAfterRestoreFailureWithinWaitKeepsDegradedFallback(t *testing.T) {
	// Default bound: a fast failure must not wait for it.
	const deviceID = "dev-restore-wait-fail"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		if cmd.Kind == vowifihost.LifecycleCommandSwitchEnd {
			return errors.New("epdg rejected")
		}
		return nil
	}

	finishSwitchWithin(t, p, deviceID)

	if p.IsESIMSwitching(deviceID) {
		t.Fatal("device still switching after the restore failed")
	}
	phases := logs.phasesAfterDone(t)
	if !slices.Contains(phases, string(esim.SwitchPhaseDegraded)) {
		t.Fatalf("phases=%v, want degraded after the restore failed", phases)
	}
	failures := logs.fields(t, restoreWaitFailedMsg)
	if len(failures) != 1 || !strings.Contains(fmt.Sprint(failures[0]["err"]), "epdg rejected") {
		t.Fatalf("restore failure logs=%v, want one with the SwitchEnd error", failures)
	}
	if want := []backend.OperatingMode{backend.ModeOnline}; !reflect.DeepEqual(be.setCalls, want) {
		t.Fatalf("setCalls=%v want %v from the radio/data fallback", be.setCalls, want)
	}
	if len(logs.fields(t, restoreWaitExceededMsg)) != 0 {
		t.Fatal("fast failure was treated as exceeding the restore wait")
	}
}

func TestHandleESIMSwitchAfterRestoreSuccessWithinWaitUnchanged(t *testing.T) {
	// Default bound: a fast success must not wait for it.
	const deviceID = "dev-restore-wait-ok"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	var switchEnds int
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		if cmd.Kind == vowifihost.LifecycleCommandSwitchEnd {
			switchEnds++
		}
		return nil
	}

	finishSwitchWithin(t, p, deviceID)

	if switchEnds != 1 {
		t.Fatalf("switch_end runs=%d, want 1", switchEnds)
	}
	if p.IsESIMSwitching(deviceID) {
		t.Fatal("device still switching after the restore succeeded")
	}
	phases := logs.phasesAfterDone(t)
	if slices.Contains(phases, string(esim.SwitchPhaseDegraded)) {
		t.Fatalf("phases=%v, want no degraded after the restore succeeded", phases)
	}
	if len(be.setCalls) != 0 {
		t.Fatalf("setCalls=%v want none because switch_end owns the restore", be.setCalls)
	}
	if len(logs.fields(t, restoreWaitExceededMsg)) != 0 {
		t.Fatal("fast success was treated as exceeding the restore wait")
	}
}

// holdSwitchEnd makes the restored start outlast the wait and end with result
// once released.
func holdSwitchEnd(p *Pool, result error) (<-chan struct{}, func()) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		if cmd.Kind != vowifihost.LifecycleCommandSwitchEnd {
			return nil
		}
		once.Do(func() { close(started) })
		select {
		case <-release:
			return result
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return started, func() { close(release) }
}

func assertBackgroundRestoreLog(t *testing.T, fields map[string]any) {
	t.Helper()
	elapsed, _ := fields["elapsed_ms"].(float64)
	if fields["switch_token"] != float64(restoreWaitSwitchToken) || elapsed < 50 {
		t.Fatalf("background restore log fields=%v, want switch_token=%d elapsed_ms>=50", fields, restoreWaitSwitchToken)
	}
}

func TestNewerESIMSwitchDuringRestoreWaitCancelsOlderRestore(t *testing.T) {
	// Default bound: the newer switch begins well inside it.
	const deviceID = "dev-restore-wait-newer-switch"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	started, stopped := blockSwitchEndUntilCanceled(p)
	olderDone := make(chan struct{})
	go func() {
		p.handleESIMSwitchAfter(deviceID, restoreWaitSwitchToken)
		close(olderDone)
	}()
	waitSupersededRestoreSignal(t, started, "post-switch SwitchEnd was not submitted")

	newerToken := make(chan uint64, 1)
	go func() { newerToken <- p.handleESIMSwitchBefore(deviceID, "") }()
	var newer uint64
	select {
	case newer = <-newerToken:
	case <-time.After(restoreWaitDeadline):
		t.Fatalf("newer switch did not begin within %s", restoreWaitDeadline)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("older restore run ctx err=%v, want context.Canceled", err)
		}
	case <-time.After(restoreWaitDeadline):
		t.Fatal("newer switch did not cancel the older restore")
	}
	waitSupersededRestoreSignal(t, olderDone, "older post-switch flow did not return inside the restore wait")

	got, ok := currentSwitchSnapshotSwitchRestore(t, p, deviceID)
	if !ok || got.SwitchToken != newer || newer == restoreWaitSwitchToken {
		t.Fatalf("current switch ok=%v token=%d, want newer token %d", ok, got.SwitchToken, newer)
	}
	if got.Phase != esim.SwitchPhasePrepare {
		t.Fatalf("newer switch phase=%q, want %q untouched by the older restore", got.Phase, esim.SwitchPhasePrepare)
	}
	if len(be.setCalls) != 0 {
		t.Fatalf("setCalls=%v, want no radio/data fallback from the older snapshot", be.setCalls)
	}
	superseded := logs.waitFields(t, postSwitchRestoreLeftMsg, nil)
	if superseded["switch_token"] != float64(restoreWaitSwitchToken) || superseded["latest_switch_token"] != float64(newer) ||
		superseded["reason"] != "newer_switch" || superseded["stage"] != "vowifi_restore_result" {
		t.Fatalf("superseded restore log fields=%v, want switch_token=%d latest_switch_token=%d reason=newer_switch stage=vowifi_restore_result",
			superseded, restoreWaitSwitchToken, newer)
	}
	if len(logs.fields(t, restoreWaitFailedMsg)) != 0 || len(logs.fields(t, restoreWaitExceededMsg)) != 0 {
		t.Fatal("older restore canceled inside the wait was logged as failed or as exceeding the wait")
	}
	// The older flow stops before the overview warm-up and the done mark.
	if outer := logs.waitFields(t, restoreWaitNotOwnedMsg, nil); outer["switch_token"] != float64(restoreWaitSwitchToken) ||
		outer["latest_switch_token"] != float64(newer) || outer["current_switch_token"] != float64(newer) || outer["reason"] != "newer_switch" {
		t.Fatalf("outer stale restore log fields=%v, want switch_token=%d latest/current_switch_token=%d reason=newer_switch", outer, restoreWaitSwitchToken, newer)
	}
}

func TestBackgroundPostSwitchRestoreSuccessIsLoggedOnce(t *testing.T) {
	withPostSwitchVoWiFiRestoreWait(t, 50*time.Millisecond)
	const deviceID = "dev-restore-wait-late-ok"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	started, release := holdSwitchEnd(p, nil)

	finishSwitchWithin(t, p, deviceID)
	waitSupersededRestoreSignal(t, started, "post-switch SwitchEnd was not submitted")
	phasesAtDone := logs.phasesAfterDone(t)
	release()

	assertBackgroundRestoreLog(t, logs.waitFields(t, restoreWaitBgDoneMsg, nil))
	if n := len(logs.fields(t, restoreWaitBgDoneMsg)); n != 1 {
		t.Fatalf("background success logged %d times, want 1", n)
	}
	if len(logs.fields(t, restoreWaitBgFailedMsg)) != 0 || len(logs.fields(t, restoreWaitCanceledMsg)) != 0 {
		t.Fatal("background success also logged as failed or canceled")
	}
	if phases := logs.fields(t, restoreWaitPhaseMsg); len(phases) != len(phasesAtDone) {
		t.Fatalf("phase updates after done: %v", phases[len(phasesAtDone):])
	}
	if len(be.setCalls) != 0 {
		t.Fatalf("setCalls=%v, want no radio/data fallback after a background success", be.setCalls)
	}
}

// A restore left running that fails later gets main's handling of a failed
// restore: the degraded mark, which finds no switch record by then, and the
// snapshot radio/data.
func TestBackgroundPostSwitchRestoreFailureGetsTheFallback(t *testing.T) {
	withPostSwitchVoWiFiRestoreWait(t, 50*time.Millisecond)
	const deviceID = "dev-restore-wait-late-fail"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	modes := watchModes(be)
	started, release := holdSwitchEnd(p, errors.New("epdg rejected"))

	finishSwitchWithin(t, p, deviceID)
	waitSupersededRestoreSignal(t, started, "post-switch SwitchEnd was not submitted")
	phasesAtDone := logs.phasesAfterDone(t)
	requireNoMode(t, modes)
	release()

	waitBackgroundRestores(t)
	failed := logs.waitFields(t, restoreWaitBgFailedMsg, nil)
	assertBackgroundRestoreLog(t, failed)
	if !strings.Contains(fmt.Sprint(failed["err"]), "epdg rejected") {
		t.Fatalf("background failure log fields=%v, want the SwitchEnd error", failed)
	}
	if n := len(logs.fields(t, restoreWaitBgFailedMsg)); n != 1 {
		t.Fatalf("background failure logged %d times, want 1", n)
	}
	handled := logs.waitFields(t, restoreWaitFailedMsg, stageIs("background_restore"))
	if handled["switch_token"] != float64(restoreWaitSwitchToken) || !strings.Contains(fmt.Sprint(handled["err"]), "epdg rejected") {
		t.Fatalf("restore failure log fields=%v, want switch_token=%d and the SwitchEnd error", handled, restoreWaitSwitchToken)
	}
	requireMode(t, modes, backend.ModeOnline)
	requireNoMode(t, modes)
	if phases := logs.fields(t, restoreWaitPhaseMsg); len(phases) != len(phasesAtDone) {
		t.Fatalf("phase updates after done: %v", phases[len(phasesAtDone):])
	}
	if slices.Contains(phasesAtDone, string(esim.SwitchPhaseDegraded)) {
		t.Fatalf("phases=%v, want no degraded before the background restore failed", phasesAtDone)
	}
	if len(logs.fields(t, postSwitchRestoreLeftMsg)) != 0 {
		t.Fatal("background failure of the last switch was treated as superseded")
	}
}

// Stopping the pool cancels a restore left running; it then leaves the modem
// alone instead of starting a fallback while the service exits.
func TestPoolContextCancelsPostSwitchRestoreLeftRunning(t *testing.T) {
	withPostSwitchVoWiFiRestoreWait(t, 50*time.Millisecond)
	const deviceID = "dev-restore-wait-pool-stop"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	modes := watchModes(be)
	_, stopped := blockSwitchEndUntilCanceled(p)

	finishSwitchWithin(t, p, deviceID)
	p.cancel()

	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("restore run ctx err=%v, want context.Canceled", err)
		}
	case <-time.After(restoreWaitDeadline):
		t.Fatal("canceling the pool context did not cancel the restore left running")
	}
	waitBackgroundRestores(t)
	assertBackgroundRestoreLog(t, logs.waitFields(t, restoreWaitCanceledMsg, nil))
	if len(logs.fields(t, restoreWaitFailedMsg)) != 0 {
		t.Fatal("restore canceled by the pool stop went on to the fallback")
	}
	requireNoMode(t, modes)
}

func TestRestoreResultByTakesResultReadyAtDeadline(t *testing.T) {
	want := errors.New("epdg rejected")
	// Both channels are ready, so a single select would pick the timer about
	// half of the time.
	for i := 0; i < 100; i++ {
		result := make(chan error, 1)
		result <- want
		expired := make(chan time.Time, 1)
		expired <- time.Now()
		if finished, err := restoreResultBy(result, expired); !finished || !errors.Is(err, want) {
			t.Fatalf("attempt %d: finished=%v err=%v, want the ready result", i, finished, err)
		}
	}
	expired := make(chan time.Time, 1)
	expired <- time.Now()
	if finished, err := restoreResultBy(make(chan error, 1), expired); finished || err != nil {
		t.Fatalf("no result: finished=%v err=%v, want left to the background", finished, err)
	}
}

// A SwitchEnd canceled within the wait by anything but a newer switch, a rebuilt
// worker or a stopping pool is handled as main handles a failed one: degraded
// mark and the snapshot radio/data.
func TestPostSwitchRestoreCanceledWithinWaitGetsTheFallback(t *testing.T) {
	const deviceID = "dev-restore-wait-canceled-current"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	_, release := holdSwitchEnd(p, context.Canceled)
	release()

	p.restorePostSwitchConnectivity(deviceID, p.GetWorker(deviceID), esimSwitchContext{SwitchToken: restoreWaitSwitchToken}, nil, true)

	if got, ok := currentSwitchSnapshotSwitchRestore(t, p, deviceID); !ok || got.Phase != esim.SwitchPhaseDegraded {
		t.Fatalf("current switch ok=%v phase=%q, want degraded", ok, got.Phase)
	}
	failed := logs.waitFields(t, restoreWaitFailedMsg, stageIs("vowifi_restore_result"))
	if !strings.Contains(fmt.Sprint(failed["err"]), context.Canceled.Error()) {
		t.Fatalf("restore failure log fields=%v, want the cancel", failed)
	}
	if want := []backend.OperatingMode{backend.ModeOnline}; !reflect.DeepEqual(be.setCalls, want) {
		t.Fatalf("setCalls=%v want %v from the radio/data fallback", be.setCalls, want)
	}
	if len(logs.fields(t, postSwitchRestoreLeftMsg)) != 0 {
		t.Fatal("canceled restore of the last switch was treated as superseded")
	}
}

// A newer switch that begins while SwitchEnd runs takes the device, whether it
// is still running or already finished when SwitchEnd returns: the older
// restore marks nothing degraded and restores no radio/data.
func TestPostSwitchRestoreSupersededWithinWaitLeavesTheNewerSwitchAlone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   error
		finished bool
		level    string
	}{
		{name: "newer-running-failed", result: errors.New("epdg rejected"), level: "warn"},
		{name: "newer-running-canceled", result: context.Canceled, level: "info"},
		{name: "newer-finished-failed", result: errors.New("epdg rejected"), finished: true, level: "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deviceID := "dev-restore-wait-" + tc.name
			logs := collectRestoreWaitLogs(t, deviceID)
			p, be := newRestoreWaitSwitch(t, deviceID)
			started, release := holdSwitchEnd(p, tc.result)
			worker := p.GetWorker(deviceID)
			done := make(chan struct{})
			go func() {
				p.restorePostSwitchConnectivity(deviceID, worker, esimSwitchContext{SwitchToken: restoreWaitSwitchToken}, nil, true)
				close(done)
			}()
			waitSupersededRestoreSignal(t, started, "post-switch SwitchEnd was not submitted")
			newer := p.beginESIMSwitch(deviceID, "").SwitchToken
			wantCurrent := newer
			if tc.finished {
				p.clearESIMSwitchIfToken(deviceID, newer)
				wantCurrent = 0
			}
			release()
			waitSupersededRestoreSignal(t, done, "restore did not return after SwitchEnd ended")

			got, ok := currentSwitchSnapshotSwitchRestore(t, p, deviceID)
			if ok == tc.finished || (ok && (got.SwitchToken != newer || got.Phase != esim.SwitchPhasePrepare)) {
				t.Fatalf("current switch ok=%v token=%d phase=%q, want newer switch %d untouched", ok, got.SwitchToken, got.Phase, newer)
			}
			if len(be.setCalls) != 0 {
				t.Fatalf("setCalls=%v, want no radio/data fallback", be.setCalls)
			}
			left := logs.waitFields(t, postSwitchRestoreLeftMsg, nil)
			if left["switch_token"] != float64(restoreWaitSwitchToken) || left["latest_switch_token"] != float64(newer) ||
				left["current_switch_token"] != float64(wantCurrent) || left["reason"] != "newer_switch" ||
				left["stage"] != "vowifi_restore_result" || !strings.Contains(fmt.Sprint(left["err"]), tc.result.Error()) {
				t.Fatalf("left restore log fields=%v, want switch_token=%d latest_switch_token=%d current_switch_token=%d reason=newer_switch err=%v",
					left, restoreWaitSwitchToken, newer, wantCurrent, tc.result)
			}
			if got := logs.level(postSwitchRestoreLeftMsg); !strings.EqualFold(got, tc.level) {
				t.Fatalf("left restore log level=%q, want %s", got, tc.level)
			}
			if len(logs.fields(t, restoreWaitFailedMsg)) != 0 {
				t.Fatal("superseded restore was logged as a failure of the current switch")
			}
		})
	}
}

// The switch watchdog can clear the switch record while SwitchEnd runs. No
// newer switch took the device, so a failed restore gets main's handling: the
// degraded mark finds no record, the snapshot radio/data still runs.
func TestPostSwitchRestoreFailureAfterWatchdogGetsTheFallback(t *testing.T) {
	const deviceID = "dev-restore-wait-record-cleared"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	started, release := holdSwitchEnd(p, errors.New("epdg rejected"))
	worker := p.GetWorker(deviceID)
	done := make(chan struct{})
	go func() {
		p.restorePostSwitchConnectivity(deviceID, worker, esimSwitchContext{SwitchToken: restoreWaitSwitchToken}, nil, true)
		close(done)
	}()
	waitSupersededRestoreSignal(t, started, "post-switch SwitchEnd was not submitted")
	clearSwitchRecordLikeWatchdog(p, deviceID)
	release()
	waitSupersededRestoreSignal(t, done, "restore did not return after SwitchEnd ended")

	failed := logs.waitFields(t, restoreWaitFailedMsg, stageIs("vowifi_restore_result"))
	if failed["switch_token"] != float64(restoreWaitSwitchToken) || !strings.Contains(fmt.Sprint(failed["err"]), "epdg rejected") {
		t.Fatalf("restore failure log fields=%v, want switch_token=%d and the SwitchEnd error", failed, restoreWaitSwitchToken)
	}
	if got := logs.level(restoreWaitFailedMsg); !strings.EqualFold(got, "error") {
		t.Fatalf("restore failure log level=%q, want error", got)
	}
	if want := []backend.OperatingMode{backend.ModeOnline}; !reflect.DeepEqual(be.setCalls, want) {
		t.Fatalf("setCalls=%v want %v from the radio/data fallback", be.setCalls, want)
	}
	if len(logs.fields(t, postSwitchRestoreLeftMsg)) != 0 {
		t.Fatal("restore after the watchdog was treated as superseded")
	}
	if p.IsESIMSwitching(deviceID) {
		t.Fatal("the degraded mark brought back the cleared switch record")
	}
}

// A zero switch token has no switch record to compare, so a failed restore keeps
// the error log and the radio/data fallback it had before the token checks.
func TestPostSwitchRestoreFailureWithZeroTokenKeepsTheFallback(t *testing.T) {
	const deviceID = "dev-restore-wait-zero-token"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	clearSwitchRecordLikeWatchdog(p, deviceID)
	_, release := holdSwitchEnd(p, errors.New("epdg rejected"))
	release()

	p.restorePostSwitchConnectivity(deviceID, p.GetWorker(deviceID), esimSwitchContext{}, nil, true)

	failures := logs.waitFields(t, restoreWaitFailedMsg, nil)
	if !strings.Contains(fmt.Sprint(failures["err"]), "epdg rejected") {
		t.Fatalf("restore failure log fields=%v, want the SwitchEnd error", failures)
	}
	if len(logs.fields(t, postSwitchRestoreLeftMsg)) != 0 {
		t.Fatal("zero-token restore failure was treated as superseded")
	}
	if got := logs.level(restoreWaitFailedMsg); !strings.EqualFold(got, "error") {
		t.Fatalf("restore failure log level=%q, want error", got)
	}
	if want := []backend.OperatingMode{backend.ModeOnline}; !reflect.DeepEqual(be.setCalls, want) {
		t.Fatalf("setCalls=%v want %v from the radio/data fallback", be.setCalls, want)
	}
}

// The gate failure path makes the same zero-token exception.
func TestPostSwitchRestoreGateFailureWithZeroTokenKeepsTheFallback(t *testing.T) {
	const deviceID = "dev-restore-gate-zero-token"
	collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	p.switchMu.Lock()
	delete(p.switchContexts, deviceID)
	delete(p.switchingDevices, deviceID)
	delete(p.switchTokens, deviceID)
	p.switchMu.Unlock()

	p.restorePostSwitchConnectivity(deviceID, p.GetWorker(deviceID), esimSwitchContext{}, errors.New("sim auth not ready"), true)

	if want := []backend.OperatingMode{backend.ModeOnline}; !reflect.DeepEqual(be.setCalls, want) {
		t.Fatalf("setCalls=%v want %v from the radio/data fallback", be.setCalls, want)
	}
}

// blockFirstIMSIRead holds the first live IMSI read, the one the Lebara home
// identity wait makes, until release is closed. Later reads pass straight on.
type blockFirstIMSIRead struct {
	*esimSwitchRestoreBackendStub
	read    atomic.Bool
	reading chan struct{}
	release chan struct{}
}

func (b *blockFirstIMSIRead) GetIMSILive(ctx context.Context) (string, error) {
	if b.read.CompareAndSwap(false, true) {
		close(b.reading)
		<-b.release
	}
	return b.esimSwitchRestoreBackendStub.GetIMSILive(ctx)
}

// A newer switch that begins and finishes while an older restore sits in the
// Lebara home identity wait clears its switch record, so the current switch
// token reads 0 as after the watchdog. The older restore still sees the newer
// switch and leaves the device alone, whether its wait then passes or ends on a
// flipped IMSI: no SwitchEnd, no desired recover state cleared, no radio/data
// and no Lebara radio guard from its snapshot on the newer switch's card.
func TestLebaraIdentityWaitOutlivingANewerSwitchLeavesItAlone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lateIMSI string
		wait     time.Duration
		wantErr  error
	}{
		{name: "home", lateIMSI: LebaraUKHomeIMSIPrefix + "0000000001", wait: restoreWaitDeadline},
		{name: "flipped", lateIMSI: "204040000000001", wait: 300 * time.Millisecond, wantErr: ErrLebaraUKFlippedIMSI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFastLebaraUKIdentityWait(t)
			// The older wait is held across the whole newer switch.
			lebaraUKIdentityWaitTimeout = tc.wait
			deviceID := "dev-restore-lebara-newer-finished-" + tc.name
			const targetICCID = "8944000000000000087"
			logs := collectRestoreWaitLogs(t, deviceID)
			p, be := newRestoreWaitSwitch(t, deviceID)
			be.liveICCID = targetICCID
			be.liveIMSI = LebaraUKHomeIMSIPrefix + "0000000001"
			held := &blockFirstIMSIRead{esimSwitchRestoreBackendStub: be, reading: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			releaseHeld := func() { releaseOnce.Do(func() { close(held.release) }) }
			// A failure before the release would leave the older restore blocked.
			t.Cleanup(releaseHeld)
			worker := p.GetWorker(deviceID)
			worker.Backend = held
			var switchEnds atomic.Int32
			p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
				if cmd.Kind == vowifihost.LifecycleCommandSwitchEnd {
					switchEnds.Add(1)
				}
				return nil
			}
			older := esimSwitchContext{SwitchToken: restoreWaitSwitchToken, TargetICCID: targetICCID, TargetLebaraCandidate: true}
			olderDone := make(chan struct{})
			go func() {
				p.restorePostSwitchConnectivity(deviceID, worker, older, nil, true)
				close(olderDone)
			}()
			waitSupersededRestoreSignal(t, held.reading, "older restore did not reach the Lebara identity wait")

			// A newer switch back to the same profile runs from begin to finish.
			newer := p.handleESIMSwitchBefore(deviceID, targetICCID)
			p.handleESIMSwitchAfter(deviceID, newer)
			if newer <= restoreWaitSwitchToken || p.IsESIMSwitching(deviceID) || p.currentSwitchToken(deviceID) != 0 {
				t.Fatalf("newer switch %d not finished: switching=%v current token=%d", newer, p.IsESIMSwitching(deviceID), p.currentSwitchToken(deviceID))
			}
			endsAfterNewer := switchEnds.Load()
			modesAfterNewer := len(be.setCalls)
			p.voWiFiHost().MarkDesiredRecoverFailed(deviceID, time.Now(), errors.New("backoff left by the newer switch"))
			be.liveIMSI = tc.lateIMSI

			releaseHeld()
			waitSupersededRestoreSignal(t, olderDone, "older restore did not return after its Lebara wait")

			if n := switchEnds.Load(); n != endsAfterNewer {
				t.Fatalf("older restore submitted %d SwitchEnd commands after the newer switch, want 0", n-endsAfterNewer)
			}
			if !p.voWiFiHost().HasDesiredRecoverState(deviceID) {
				t.Fatal("older restore cleared the desired recover state the newer switch left")
			}
			if got := be.setCalls[modesAfterNewer:]; len(got) != 0 {
				t.Fatalf("older restore set operating modes %v, want the radio left to the newer switch", got)
			}
			left := logs.waitFields(t, postSwitchRestoreLeftMsg, func(fields map[string]any) bool {
				return fields["switch_token"] == float64(restoreWaitSwitchToken)
			})
			if left["latest_switch_token"] != float64(newer) || left["current_switch_token"] != float64(0) ||
				left["reason"] != "newer_switch" || left["stage"] != "vowifi_restore_begin" {
				t.Fatalf("left restore log fields=%v, want latest_switch_token=%d current_switch_token=0 reason=newer_switch stage=vowifi_restore_begin", left, newer)
			}
			// The flipped IMSI error reaches the check, which comes before the
			// Lebara recover it would start.
			if tc.wantErr != nil && !strings.Contains(fmt.Sprint(left["err"]), tc.wantErr.Error()) {
				t.Fatalf("left restore log err=%v, want %v", left["err"], tc.wantErr)
			}
		})
	}
}

// A worker rebuilt while SwitchEnd runs belongs to no switch the old restore
// knows about: a failed restore marks nothing degraded and restores no radio/data.
func TestPostSwitchRestoreFailureAfterWorkerReplacedLeavesTheNewWorkerAlone(t *testing.T) {
	const deviceID = "dev-restore-worker-replaced"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	started, release := holdSwitchEnd(p, errors.New("epdg rejected"))
	worker := p.GetWorker(deviceID)
	done := make(chan struct{})
	go func() {
		p.restorePostSwitchConnectivity(deviceID, worker, esimSwitchContext{SwitchToken: restoreWaitSwitchToken}, nil, true)
		close(done)
	}()
	waitSupersededRestoreSignal(t, started, "post-switch SwitchEnd was not submitted")
	replacementBackend := &esimSwitchRestoreBackendStub{mode: backend.BackendQMI, getMode: backend.ModeOnline}
	p.mu.Lock()
	p.workers[deviceID] = &Worker{ID: deviceID, Config: config.DeviceConfig{ID: deviceID, VoWiFiEnabled: true}, Backend: replacementBackend}
	p.mu.Unlock()
	release()
	waitSupersededRestoreSignal(t, done, "restore did not return after SwitchEnd ended")

	got, ok := currentSwitchSnapshotSwitchRestore(t, p, deviceID)
	if !ok || got.Phase == esim.SwitchPhaseDegraded {
		t.Fatalf("current switch ok=%v phase=%q, want not degraded", ok, got.Phase)
	}
	if len(be.setCalls) != 0 || len(replacementBackend.setCalls) != 0 {
		t.Fatalf("setCalls old=%v new=%v, want no radio/data fallback", be.setCalls, replacementBackend.setCalls)
	}
	replaced := logs.waitFields(t, postSwitchRestoreLeftMsg, nil)
	if replaced["reason"] != "worker_changed" || replaced["stage"] != "vowifi_restore_result" ||
		!strings.Contains(fmt.Sprint(replaced["err"]), "epdg rejected") {
		t.Fatalf("worker replaced log fields=%v, want reason=worker_changed stage=vowifi_restore_result and the SwitchEnd error", replaced)
	}
	if got := logs.level(postSwitchRestoreLeftMsg); !strings.EqualFold(got, "warn") {
		t.Fatalf("worker replaced log level=%q, want warn", got)
	}
	if len(logs.fields(t, restoreWaitFailedMsg)) != 0 {
		t.Fatal("restore failure on a replaced worker was logged as a failure of the current worker")
	}
}

// lebaraRestoreWaitSwitch is newRestoreWaitSwitch on a Lebara UK card, whose
// radio must stay off after a switch: in low power some modems come back
// online on another IMSI. modes receives every operating mode the restore sets.
func lebaraRestoreWaitSwitch(t *testing.T, deviceID string) (*Pool, esimSwitchContext, <-chan backend.OperatingMode) {
	t.Helper()
	withFastLebaraUKIdentityWait(t)
	p, be := newRestoreWaitSwitch(t, deviceID)
	modes := make(chan backend.OperatingMode, 16)
	be.setModeHook = func(mode backend.OperatingMode) { modes <- mode }
	// The home identity wait passes at once, so the restore reaches SwitchEnd.
	be.liveIMSI = LebaraUKHomeIMSIPrefix + "0000000001"
	snapshot := esimSwitchContext{SwitchToken: restoreWaitSwitchToken, TargetICCID: be.liveICCID}
	withSwitchSnapshot(p, deviceID, snapshot)
	w := p.GetWorker(deviceID)
	w.cacheMu.Lock()
	w.state.Identity.IMSI = be.liveIMSI
	w.cacheMu.Unlock()
	return p, snapshot, modes
}

// requireRadioOff checks every operating mode a finished restore set: at least
// one, and all of them persistent radio off.
func requireRadioOff(t *testing.T, modes <-chan backend.OperatingMode) {
	t.Helper()
	var got []backend.OperatingMode
	for {
		select {
		case mode := <-modes:
			got = append(got, mode)
			continue
		default:
		}
		break
	}
	if len(got) == 0 {
		t.Fatal("restore did not keep the Lebara radio off")
	}
	for _, mode := range got {
		if mode != backend.ModeRFOff {
			t.Fatalf("restore set operating modes %v on a Lebara card, want only %v", got, backend.ModeRFOff)
		}
	}
}

// Every snapshot fallback that a restore of the last switch reaches starts with
// main's Lebara guard, so a Lebara card's radio stays off however the restore
// ends. A background restore that gets canceled starts no fallback; see
// TestBackgroundPostSwitchRestoreCanceledLeavesTheRadioToTheCanceler.
func TestLebaraRadioStaysOffOnEveryFallback(t *testing.T) {
	restore := func(p *Pool, deviceID string, snapshot esimSwitchContext, gateErr error) {
		p.restorePostSwitchConnectivity(deviceID, p.GetWorker(deviceID), snapshot, gateErr, true)
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, p *Pool, deviceID string, snapshot esimSwitchContext)
	}{
		{name: "failed", run: func(t *testing.T, p *Pool, deviceID string, snapshot esimSwitchContext) {
			_, release := holdSwitchEnd(p, errors.New("epdg rejected"))
			release()
			restore(p, deviceID, snapshot, nil)
		}},
		{name: "canceled", run: func(t *testing.T, p *Pool, deviceID string, snapshot esimSwitchContext) {
			_, release := holdSwitchEnd(p, context.Canceled)
			release()
			restore(p, deviceID, snapshot, nil)
		}},
		{name: "watchdog", run: func(t *testing.T, p *Pool, deviceID string, snapshot esimSwitchContext) {
			started, release := holdSwitchEnd(p, errors.New("epdg rejected"))
			done := make(chan struct{})
			go func() {
				restore(p, deviceID, snapshot, nil)
				close(done)
			}()
			waitSupersededRestoreSignal(t, started, "post-switch SwitchEnd was not submitted")
			clearSwitchRecordLikeWatchdog(p, deviceID)
			release()
			waitSupersededRestoreSignal(t, done, "restore did not return after SwitchEnd ended")
		}},
		{name: "gate", run: func(t *testing.T, p *Pool, deviceID string, snapshot esimSwitchContext) {
			restore(p, deviceID, snapshot, errors.New("sim auth not ready"))
		}},
		{name: "background-failed", run: func(t *testing.T, p *Pool, deviceID string, snapshot esimSwitchContext) {
			withPostSwitchVoWiFiRestoreWait(t, 50*time.Millisecond)
			_, release := holdSwitchEnd(p, errors.New("epdg rejected"))
			finishSwitchWithin(t, p, deviceID)
			if p.IsESIMSwitching(deviceID) {
				t.Fatal("device still switching after the restore outlasted the wait")
			}
			release()
			waitBackgroundRestores(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deviceID := "dev-restore-lebara-" + tc.name
			collectRestoreWaitLogs(t, deviceID)
			p, snapshot, modes := lebaraRestoreWaitSwitch(t, deviceID)
			tc.run(t, p, deviceID, snapshot)
			requireRadioOff(t, modes)
		})
	}
}

// A newer switch owns the radio: the older restore leaves it alone, Lebara card
// or not.
func TestLebaraRadioLeftToANewerSwitch(t *testing.T) {
	const deviceID = "dev-restore-lebara-newer-switch"
	collectRestoreWaitLogs(t, deviceID)
	p, snapshot, modes := lebaraRestoreWaitSwitch(t, deviceID)
	started, release := holdSwitchEnd(p, errors.New("epdg rejected"))
	worker := p.GetWorker(deviceID)
	done := make(chan struct{})
	go func() {
		p.restorePostSwitchConnectivity(deviceID, worker, snapshot, nil, true)
		close(done)
	}()
	waitSupersededRestoreSignal(t, started, "post-switch SwitchEnd was not submitted")
	p.beginESIMSwitch(deviceID, "")
	release()
	waitSupersededRestoreSignal(t, done, "restore did not return after SwitchEnd ended")

	select {
	case mode := <-modes:
		t.Fatalf("older restore set operating mode %v, want the radio left to the newer switch", mode)
	default:
	}
}

// Only a switch begun after this one, a rebuilt worker or a stopping pool takes
// the device from a restore. A switch record the watchdog cleared still belongs
// to the last switch, and a newer switch that already finished still counts.
func TestPostSwitchRestoreSupersededReason(t *testing.T) {
	beginNewer := func(p *Pool, deviceID string) { p.beginESIMSwitch(deviceID, "") }
	replaceWorker := func(p *Pool, deviceID string) {
		replacement := &Worker{ID: deviceID}
		p.assignWorkerGeneration(replacement)
		p.mu.Lock()
		p.workers[deviceID] = replacement
		p.mu.Unlock()
	}
	for _, tc := range []struct {
		name string
		// owner captures the worker and its generation, as a production
		// snapshot does, instead of falling back to comparing the worker pointer.
		owner bool
		setup func(p *Pool, deviceID string)
		want  string
	}{
		{name: "current", setup: func(*Pool, string) {}, want: ""},
		{name: "watchdog", setup: clearSwitchRecordLikeWatchdog, want: ""},
		{name: "newer-switch", setup: beginNewer, want: "newer_switch"},
		{name: "newer-switch-finished", setup: func(p *Pool, deviceID string) {
			p.clearESIMSwitchIfToken(deviceID, p.beginESIMSwitch(deviceID, "").SwitchToken)
		}, want: "newer_switch"},
		{name: "worker-replaced", setup: func(p *Pool, deviceID string) {
			p.mu.Lock()
			p.workers[deviceID] = &Worker{ID: deviceID}
			p.mu.Unlock()
		}, want: "worker_changed"},
		{name: "pool-stopped", setup: func(p *Pool, _ string) { p.cancel() }, want: "pool_stopped"},
		{name: "owner-current", owner: true, setup: func(*Pool, string) {}, want: ""},
		{name: "owner-worker-rebuilt", owner: true, setup: replaceWorker, want: "worker_changed"},
		{name: "owner-generation-changed", owner: true, setup: func(p *Pool, deviceID string) {
			p.assignWorkerGeneration(p.GetWorker(deviceID))
		}, want: "worker_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deviceID := "dev-restore-owner-" + tc.name
			p, _ := newRestoreWaitSwitch(t, deviceID)
			worker := p.GetWorker(deviceID)
			snapshot := esimSwitchContext{SwitchToken: restoreWaitSwitchToken}
			if tc.owner {
				p.assignWorkerGeneration(worker)
				snapshot.Owner = workerLifecycleOwner{worker: worker, generation: worker.generation}
			}
			tc.setup(p, deviceID)
			if got := p.postSwitchRestoreSupersededReason(deviceID, worker, snapshot); got != tc.want {
				t.Fatalf("superseded reason=%q, want %q", got, tc.want)
			}
		})
	}
}

// A Lebara home identity wait that outlasts the switch watchdog still restores
// VoWiFi: no newer switch took the device.
func TestLebaraIdentityWaitAfterSwitchRecordClearedStillSubmitsSwitchEnd(t *testing.T) {
	withFastLebaraUKIdentityWait(t)
	const deviceID = "dev-restore-lebara-record-cleared-ok"
	collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	be.liveIMSI = LebaraUKHomeIMSIPrefix + "0000000001"
	var switchEnds atomic.Int32
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		if cmd.Kind == vowifihost.LifecycleCommandSwitchEnd {
			switchEnds.Add(1)
		}
		return nil
	}
	clearSwitchRecordLikeWatchdog(p, deviceID)
	snapshot := esimSwitchContext{SwitchToken: restoreWaitSwitchToken, TargetICCID: be.liveICCID, TargetLebaraCandidate: true}

	p.restorePostSwitchConnectivity(deviceID, p.GetWorker(deviceID), snapshot, nil, true)

	if n := switchEnds.Load(); n != 1 {
		t.Fatalf("restore after the watchdog submitted %d SwitchEnd commands, want 1", n)
	}
}

// A gate failure on a rebuilt worker marks nothing degraded and restores no
// radio/data on the old snapshot.
func TestPostSwitchRestoreGateFailureOnReplacedWorkerLeavesItAlone(t *testing.T) {
	const deviceID = "dev-restore-gate-worker-replaced"
	collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	worker := p.GetWorker(deviceID)
	replacementBackend := &esimSwitchRestoreBackendStub{mode: backend.BackendQMI, getMode: backend.ModeOnline}
	p.mu.Lock()
	p.workers[deviceID] = &Worker{ID: deviceID, Config: config.DeviceConfig{ID: deviceID, VoWiFiEnabled: true}, Backend: replacementBackend}
	p.mu.Unlock()

	p.restorePostSwitchConnectivity(deviceID, worker, esimSwitchContext{SwitchToken: restoreWaitSwitchToken}, errors.New("sim auth not ready"), true)

	got, ok := currentSwitchSnapshotSwitchRestore(t, p, deviceID)
	if !ok || got.Phase == esim.SwitchPhaseDegraded {
		t.Fatalf("current switch ok=%v phase=%q, want not degraded", ok, got.Phase)
	}
	if len(be.setCalls) != 0 || len(replacementBackend.setCalls) != 0 {
		t.Fatalf("setCalls old=%v new=%v, want no radio/data fallback", be.setCalls, replacementBackend.setCalls)
	}
}

// With VoWiFi off the restore goes straight to the snapshot radio/data; a
// rebuilt worker gets none of it.
func TestPostSwitchRadioFallbackSkippedForReplacedWorker(t *testing.T) {
	const deviceID = "dev-restore-fallback-worker-replaced"
	collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	worker := p.GetWorker(deviceID)
	worker.Config.VoWiFiEnabled = false
	replacementBackend := &esimSwitchRestoreBackendStub{mode: backend.BackendQMI, getMode: backend.ModeOnline}
	p.mu.Lock()
	p.workers[deviceID] = &Worker{ID: deviceID, Config: config.DeviceConfig{ID: deviceID}, Backend: replacementBackend}
	p.mu.Unlock()

	p.restorePostSwitchConnectivity(deviceID, worker, esimSwitchContext{SwitchToken: restoreWaitSwitchToken}, nil, true)

	if len(be.setCalls) != 0 || len(replacementBackend.setCalls) != 0 {
		t.Fatalf("setCalls old=%v new=%v, want no radio/data fallback", be.setCalls, replacementBackend.setCalls)
	}
}

// stallFirstModeSet answers the first operating mode change with a QMI stall,
// which the snapshot radio restore retries, after running onFirst.
type stallFirstModeSet struct {
	*esimSwitchRestoreBackendStub
	stalled atomic.Bool
	onFirst func()
}

func (b *stallFirstModeSet) SetOperatingMode(ctx context.Context, mode backend.OperatingMode) error {
	if err := b.esimSwitchRestoreBackendStub.SetOperatingMode(ctx, mode); err != nil || !b.stalled.CompareAndSwap(false, true) {
		return err
	}
	b.onFirst()
	return errors.New("qmi request timed out")
}

// A newer switch can begin while the snapshot radio restore waits to retry a
// stalled mode change; the retry would then reach the newer switch's card,
// possibly a Lebara one. The restore checks ownership after each retry wait.
// Without a newer switch it retries as main does.
func TestSnapshotRadioRetryStopsForANewerSwitch(t *testing.T) {
	for _, newer := range []bool{false, true} {
		name := "no-newer-switch"
		if newer {
			name = "newer-switch"
		}
		t.Run(name, func(t *testing.T) {
			original := postSwitchModeRetryDelays
			postSwitchModeRetryDelays = []time.Duration{200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond}
			t.Cleanup(func() { postSwitchModeRetryDelays = original })
			deviceID := "dev-restore-radio-retry-" + name
			logs := collectRestoreWaitLogs(t, deviceID)
			p, be := newRestoreWaitSwitch(t, deviceID)
			worker := p.GetWorker(deviceID)
			worker.Config.VoWiFiEnabled = false
			stalled := make(chan struct{})
			worker.Backend = &stallFirstModeSet{esimSwitchRestoreBackendStub: be, onFirst: func() { close(stalled) }}
			// The newer switch begins once the stalled call has returned, while the
			// restore waits to retry.
			newerToken := make(chan uint64, 1)
			go func() {
				<-stalled
				if newer {
					newerToken <- p.beginESIMSwitch(deviceID, "").SwitchToken
				}
				close(newerToken)
			}()

			p.restorePostSwitchConnectivity(deviceID, worker, esimSwitchContext{SwitchToken: restoreWaitSwitchToken}, nil, true)

			if !newer {
				if want := []backend.OperatingMode{backend.ModeOnline, backend.ModeOnline}; !reflect.DeepEqual(be.setCalls, want) {
					t.Fatalf("setCalls=%v, want %v: the stalled mode change retried once", be.setCalls, want)
				}
				return
			}
			token := <-newerToken
			if want := []backend.OperatingMode{backend.ModeOnline}; !reflect.DeepEqual(be.setCalls, want) {
				t.Fatalf("setCalls=%v, want %v: no retry after the newer switch began", be.setCalls, want)
			}
			left := logs.waitFields(t, postSwitchRestoreLeftMsg, nil)
			if left["stage"] != "radio_data_fallback_during_fallback" || left["reason"] != "newer_switch" ||
				left["latest_switch_token"] != float64(token) {
				t.Fatalf("left restore log fields=%v, want stage=radio_data_fallback_during_fallback reason=newer_switch latest_switch_token=%d", left, token)
			}
		})
	}
}

// The data step of the snapshot fallback comes after the radio step; a newer
// switch that begins in between keeps the older snapshot's data off its card.
func TestSnapshotDataStepStopsForANewerSwitch(t *testing.T) {
	for _, newer := range []bool{false, true} {
		name := "no-newer-switch"
		if newer {
			name = "newer-switch"
		}
		t.Run(name, func(t *testing.T) {
			deviceID := "dev-restore-data-step-" + name
			logs := collectRestoreWaitLogs(t, deviceID)
			p, be := newRestoreWaitSwitch(t, deviceID)
			worker := p.GetWorker(deviceID)
			worker.Config.VoWiFiEnabled = false
			var connects atomic.Int32
			worker.netOverride = &fakeController{connectHook: func() { connects.Add(1) }}
			be.setModeHook = func(mode backend.OperatingMode) {
				if newer && mode == backend.ModeOnline {
					p.beginESIMSwitch(deviceID, "")
				}
			}

			p.restorePostSwitchConnectivity(deviceID, worker, esimSwitchContext{SwitchToken: restoreWaitSwitchToken, NetworkEnabledBefore: true}, nil, true)

			if want := []backend.OperatingMode{backend.ModeOnline}; !reflect.DeepEqual(be.setCalls, want) {
				t.Fatalf("setCalls=%v, want %v", be.setCalls, want)
			}
			if !newer {
				if n := connects.Load(); n != 1 {
					t.Fatalf("data connects=%d, want 1 from the snapshot", n)
				}
				return
			}
			if n := connects.Load(); n != 0 {
				t.Fatalf("data connects=%d, want none after the newer switch began", n)
			}
			if left := logs.waitFields(t, postSwitchRestoreLeftMsg, nil); left["stage"] != "radio_data_fallback_during_fallback" || left["reason"] != "newer_switch" {
				t.Fatalf("left restore log fields=%v, want stage=radio_data_fallback_during_fallback reason=newer_switch", left)
			}
		})
	}
}

// A switch takes its token before it reads the modem for its snapshot, so an
// older restore already counts as superseded while that read runs.
func TestNewSwitchSupersedesOlderRestoreBeforeItReadsTheModem(t *testing.T) {
	const deviceID = "dev-restore-token-before-read"
	collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	worker := p.GetWorker(deviceID)
	var during string
	worker.Backend = &modeReadHook{esimSwitchRestoreBackendStub: be, onRead: func() {
		during = p.postSwitchRestoreSupersededReason(deviceID, worker, esimSwitchContext{SwitchToken: restoreWaitSwitchToken})
	}}

	p.beginESIMSwitch(deviceID, "")

	if during != "newer_switch" {
		t.Fatalf("superseded reason while the newer switch read the modem=%q, want newer_switch", during)
	}
}

// modeReadHook runs onRead on each operating mode read.
type modeReadHook struct {
	*esimSwitchRestoreBackendStub
	onRead func()
}

func (b *modeReadHook) GetOperatingMode(ctx context.Context) (backend.OperatingMode, error) {
	b.onRead()
	return b.esimSwitchRestoreBackendStub.GetOperatingMode(ctx)
}

// A stopping pool is not a failure of the restore: the error it drops, here the
// identity wait's own pool-stopped error, is logged at info.
func TestPostSwitchRestoreLeftOnPoolStopLogsInfo(t *testing.T) {
	const deviceID = "dev-restore-pool-stopped-level"
	logs := collectRestoreWaitLogs(t, deviceID)
	p, be := newRestoreWaitSwitch(t, deviceID)
	p.cancel()

	p.restorePostSwitchConnectivity(deviceID, p.GetWorker(deviceID), esimSwitchContext{SwitchToken: restoreWaitSwitchToken}, errors.New("设备池已停止"), true)

	if left := logs.waitFields(t, postSwitchRestoreLeftMsg, nil); left["reason"] != "pool_stopped" {
		t.Fatalf("left restore log fields=%v, want reason=pool_stopped", left)
	}
	if got := logs.level(postSwitchRestoreLeftMsg); !strings.EqualFold(got, "info") {
		t.Fatalf("left restore log level=%q, want info", got)
	}
	if len(be.setCalls) != 0 {
		t.Fatalf("setCalls=%v, want none while the pool stops", be.setCalls)
	}
}

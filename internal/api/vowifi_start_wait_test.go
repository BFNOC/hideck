package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/internal/vowifihost"
	"github.com/yibaiba/hideck/pkg/logger"
)

func shortenVoWiFiStartWait(t *testing.T) {
	t.Helper()
	previous := vowifiStartWaitLimit
	vowifiStartWaitLimit = 50 * time.Millisecond
	t.Cleanup(func() { vowifiStartWaitLimit = previous })
}

func TestAwaitVoWiFiStartReturnsFastResult(t *testing.T) {
	shortenVoWiFiStartWait(t)
	if pending, err := awaitVoWiFiStart("dev-1", "test", func() error { return nil }); pending || err != nil {
		t.Fatalf("fast success: pending=%v err=%v", pending, err)
	}
	want := errors.New("ike auth failed")
	if pending, err := awaitVoWiFiStart("dev-1", "test", func() error { return want }); pending || !errors.Is(err, want) {
		t.Fatalf("fast failure: pending=%v err=%v", pending, err)
	}
}

func TestAwaitVoWiFiStartBackgroundsSlowStart(t *testing.T) {
	shortenVoWiFiStartWait(t)
	for _, result := range []error{nil, errors.New("ike auth failed")} {
		var calls atomic.Int32
		release := make(chan struct{})
		returned := make(chan struct{})
		began := time.Now()
		pending, err := awaitVoWiFiStart("dev-1", "test", func() error {
			calls.Add(1)
			<-release
			close(returned)
			return result
		})
		if !pending || err != nil {
			t.Fatalf("slow start: pending=%v err=%v", pending, err)
		}
		if waited := time.Since(began); waited > 20*vowifiStartWaitLimit {
			t.Fatalf("slow start held the caller for %v", waited)
		}
		close(release)
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("backgrounded start did not finish")
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("start called %d times, want 1", got)
		}
	}
}

// waitBackgroundStarts waits until every backgrounded start has logged its
// result.
func waitBackgroundStarts(t *testing.T) {
	t.Helper()
	settled := make(chan struct{})
	go func() {
		backgroundStarts.Wait()
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Error("a backgrounded VoWiFi start is still running")
	}
}

// captureStartWaitLogs routes the global logger to the log stream for one test
// and puts the previous logger back afterwards. Backgrounded starts, the test's
// own and those left by earlier tests, finish logging before either swap.
func captureStartWaitLogs(t *testing.T) <-chan logger.LogEntry {
	t.Helper()
	waitBackgroundStarts(t)
	prevLog, prevSugar := logger.ZapLogger(), logger.SugarLogger()
	logger.Setup(logger.LogConfig{Filename: filepath.Join(t.TempDir(), "app.log")})
	ch := logger.GlobalBroadcaster.Subscribe()
	t.Cleanup(func() {
		waitBackgroundStarts(t)
		logger.GlobalBroadcaster.Unsubscribe(ch)
		logger.Log, logger.Sugar = prevLog, prevSugar
	})
	return ch
}

// waitLog returns the fields of the next entry logged as message, device
// prefix included, and checks its level.
func waitLog(t *testing.T, ch <-chan logger.LogEntry, level, message string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case entry := <-ch:
			if entry.Message != message {
				continue
			}
			if entry.Level != level {
				t.Fatalf("%q logged at %s, want %s", message, entry.Level, level)
			}
			fields := map[string]any{}
			if entry.Fields != "" {
				if err := json.Unmarshal([]byte(entry.Fields), &fields); err != nil {
					t.Fatalf("decode fields %s: %v", entry.Fields, err)
				}
			}
			return fields
		case <-deadline:
			t.Fatalf("log %q was not seen", message)
		}
	}
}

// waitStartWaitLog waits for msg logged for dev-1 with op test.
func waitStartWaitLog(t *testing.T, ch <-chan logger.LogEntry, level, msg string) map[string]any {
	t.Helper()
	fields := waitLog(t, ch, level, "[dev-1] "+msg)
	if fields["op"] != "test" {
		t.Fatalf("%q fields=%v, want op=test", msg, fields)
	}
	return fields
}

func TestAwaitVoWiFiStartLogsBackgroundResult(t *testing.T) {
	for _, tc := range []struct {
		name, level, msg string
		result           error
	}{
		{name: "success", level: "info", msg: "后台 VoWiFi 启动完成"},
		{name: "failure", level: "warn", msg: "后台 VoWiFi 启动失败", result: errors.New("ike auth failed")},
		{name: "canceled", level: "info", msg: "后台 VoWiFi 启动已取消", result: fmt.Errorf("enable: %w", context.Canceled)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortenVoWiFiStartWait(t)
			logs := captureStartWaitLogs(t)
			release := make(chan struct{})
			pending, err := awaitVoWiFiStart("dev-1", "test", func() error {
				<-release
				return tc.result
			})
			if !pending || err != nil {
				t.Fatalf("slow start: pending=%v err=%v", pending, err)
			}
			fields := waitStartWaitLog(t, logs, "warn", "VoWiFi 启动未在限时内完成，释放设备锁后在后台继续")
			limit := float64(vowifiStartWaitLimit.Milliseconds())
			if waited, _ := fields["waited_ms"].(float64); waited < limit || fields["limit_ms"] != limit {
				t.Fatalf("timeout fields=%v, want waited_ms >= %v and limit_ms = %v", fields, limit, limit)
			}

			close(release)
			waitBackgroundStarts(t)
			fields = waitStartWaitLog(t, logs, tc.level, tc.msg)
			if _, ok := fields["elapsed_ms"].(float64); !ok {
				t.Fatalf("result fields=%v, want elapsed_ms", fields)
			}
			if tc.result != nil && fields["err"] != tc.result.Error() {
				t.Fatalf("result fields=%v, want err=%q", fields, tc.result.Error())
			}
		})
	}
}

func TestAwaitVoWiFiStartRecoversPanic(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprintf("backgrounded=%v", slow), func(t *testing.T) {
			shortenVoWiFiStartWait(t)
			logs := captureStartWaitLogs(t)
			release := make(chan struct{})
			if !slow {
				close(release)
			}
			pending, err := awaitVoWiFiStart("dev-1", "test", func() error {
				<-release
				panic("nil modem adapter")
			})
			if slow {
				if !pending || err != nil {
					t.Fatalf("slow start: pending=%v err=%v", pending, err)
				}
				close(release)
				waitBackgroundStarts(t)
			} else if pending || err == nil || !strings.Contains(err.Error(), "nil modem adapter") {
				t.Fatalf("fast panic: pending=%v err=%v, want the panic as error", pending, err)
			}
			fields := waitStartWaitLog(t, logs, "error", "VoWiFi 启动 panic，已转为错误")
			if fields["panic"] != "nil modem adapter" {
				t.Fatalf("panic fields=%v, want the panic value", fields)
			}
			if stack, _ := fields["stack"].(string); !strings.Contains(stack, "TestAwaitVoWiFiStartRecoversPanic") {
				t.Fatalf("panic stack=%q, want the panicking function", stack)
			}
			if slow {
				fields = waitStartWaitLog(t, logs, "warn", "后台 VoWiFi 启动失败")
				if e, _ := fields["err"].(string); !strings.Contains(e, "nil modem adapter") {
					t.Fatalf("background failure fields=%v, want the panic as err", fields)
				}
			}
		})
	}
}

func TestStartResultByAnswersResultReadyAtDeadline(t *testing.T) {
	want := errors.New("ike auth failed")
	// Both channels are ready, so a single select would pick the timer about
	// half of the time.
	for i := 0; i < 100; i++ {
		done := make(chan error, 1)
		done <- want
		expired := make(chan time.Time, 1)
		expired <- time.Now()
		if finished, err := startResultBy(done, expired); !finished || !errors.Is(err, want) {
			t.Fatalf("attempt %d: finished=%v err=%v, want the ready result", i, finished, err)
		}
	}
	expired := make(chan time.Time, 1)
	expired <- time.Now()
	if finished, err := startResultBy(make(chan error, 1), expired); finished || err != nil {
		t.Fatalf("no result: finished=%v err=%v, want pending", finished, err)
	}
}

// blockingVoWiFiLifecycle stands in for a runtime that keeps retrying while the
// ePDG rejects IKE_AUTH: enable/restart runs claim the start like the real
// runtime does and return only when canceled.
type blockingVoWiFiLifecycle struct {
	active, maxActive atomic.Int32
	started           chan vowifihost.LifecycleCommandKind
	canceled          chan vowifihost.LifecycleCommandKind
}

func newVoWiFiLifecycleTestServer(t *testing.T) (*gin.Engine, *blockingVoWiFiLifecycle, *device.Pool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	openTestDB(t)
	shortenVoWiFiStartWait(t)
	path := writeDeviceMgmtLimitConfig(t, `
server:
  port: ":7575"
devices:
  - id: dev-epdg
    interface: wwan0
    vowifi_enabled: true
`)
	if err := config.InitGlobalManager(path); err != nil {
		t.Fatalf("InitGlobalManager() error = %v", err)
	}
	pool := device.NewPool(&config.Config{})
	t.Cleanup(func() { _ = pool.Shutdown() })
	worker := &device.Worker{ID: "dev-epdg", Config: config.DeviceConfig{ID: "dev-epdg", VoWiFiEnabled: true}}
	setNestedPrivateField(t, worker, []string{"state", "Identity", "ICCID"}, "8944000000000000123")
	injectWorker(pool, worker)

	lifecycle := &blockingVoWiFiLifecycle{
		started:  make(chan vowifihost.LifecycleCommandKind, 16),
		canceled: make(chan vowifihost.LifecycleCommandKind, 16),
	}
	pool.SetVoWiFiLifecycleRunForTest(func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		if cmd.Kind == vowifihost.LifecycleCommandDisable {
			return nil
		}
		n := lifecycle.active.Add(1)
		defer lifecycle.active.Add(-1)
		for {
			peak := lifecycle.maxActive.Load()
			if n <= peak || lifecycle.maxActive.CompareAndSwap(peak, n) {
				break
			}
		}
		pool.BeginVoWiFiStartForTest(cmd.DeviceID)
		lifecycle.started <- cmd.Kind
		<-ctx.Done()
		lifecycle.canceled <- cmd.Kind
		return ctx.Err()
	})

	s := &Server{pool: pool, configPath: path}
	r := gin.New()
	r.POST("/api/devices/:device_id/vowifi/actions/reconnect", s.handleDeviceMgmtReconnectVoWiFi)
	r.PATCH("/api/devices/:device_id/vowifi", s.handleDeviceVoWiFiPatch)
	r.PATCH("/api/devices/:device_id/network", s.handleDeviceNetworkPatch)
	r.PATCH("/api/devices/:device_id/flight-mode", s.handleDeviceMgmtSetFlightMode)
	return r, lifecycle, pool
}

// serveWithDeadline fails instead of hanging when a handler never returns.
func serveWithDeadline(t *testing.T, r *gin.Engine, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s %s did not return while the VoWiFi start was blocked", method, target)
	}
	return rec
}

func requirePendingOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body=%s: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusOK || body["status"] != "ok" || body["pending"] != true {
		t.Fatalf("status=%d body=%s, want 200 pending", rec.Code, rec.Body.String())
	}
}

func waitLifecycleKind(t *testing.T, ch chan vowifihost.LifecycleCommandKind, what string, want vowifihost.LifecycleCommandKind) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("%s kind=%s want %s", what, got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("lifecycle run %s was not observed", what)
	}
}

func TestVoWiFiStartReleasesDeviceLocksWhileTunnelRetries(t *testing.T) {
	tests := []struct {
		name, method, target, body string
		kind                       vowifihost.LifecycleCommandKind
	}{
		{name: "reconnect", method: http.MethodPost, target: "/api/devices/dev-epdg/vowifi/actions/reconnect", kind: vowifihost.LifecycleCommandRestart},
		{name: "enable", method: http.MethodPatch, target: "/api/devices/dev-epdg/vowifi", body: `{"enabled":true}`, kind: vowifihost.LifecycleCommandEnable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, lifecycle, _ := newVoWiFiLifecycleTestServer(t)

			requirePendingOK(t, serveWithDeadline(t, r, test.method, test.target, test.body))
			waitLifecycleKind(t, lifecycle.started, "start", test.kind)

			rec := serveWithDeadline(t, r, http.MethodPatch, "/api/devices/dev-epdg/vowifi", `{"enabled":false}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
			}
			waitLifecycleKind(t, lifecycle.canceled, "cancel", test.kind)
		})
	}
}

func TestBackToBackReconnectsRunOneLifecycleAtATime(t *testing.T) {
	r, lifecycle, _ := newVoWiFiLifecycleTestServer(t)

	for i := 0; i < 2; i++ {
		requirePendingOK(t, serveWithDeadline(t, r, http.MethodPost, "/api/devices/dev-epdg/vowifi/actions/reconnect", ""))
		waitLifecycleKind(t, lifecycle.started, "start", vowifihost.LifecycleCommandRestart)
	}
	waitLifecycleKind(t, lifecycle.canceled, "first cancel", vowifihost.LifecycleCommandRestart)
	if got := lifecycle.maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent lifecycle runs=%d want 1", got)
	}

	rec := serveWithDeadline(t, r, http.MethodPatch, "/api/devices/dev-epdg/vowifi", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	waitLifecycleKind(t, lifecycle.canceled, "second cancel", vowifihost.LifecycleCommandRestart)
	if got := lifecycle.maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent lifecycle runs=%d want 1", got)
	}
}

const testVoWiFiPath = "/api/devices/dev-epdg/vowifi"

// startBackgroundedEnable leaves an enable running in the background, the way
// an ePDG that keeps rejecting IKE_AUTH does.
func startBackgroundedEnable(t *testing.T, r *gin.Engine, lifecycle *blockingVoWiFiLifecycle) {
	t.Helper()
	requirePendingOK(t, serveWithDeadline(t, r, http.MethodPatch, testVoWiFiPath, `{"enabled":true}`))
	waitLifecycleKind(t, lifecycle.started, "start", vowifihost.LifecycleCommandEnable)
}

func disableAndRequireCanceled(t *testing.T, r *gin.Engine, lifecycle *blockingVoWiFiLifecycle) {
	t.Helper()
	rec := serveWithDeadline(t, r, http.MethodPatch, testVoWiFiPath, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	waitLifecycleKind(t, lifecycle.canceled, "cancel", vowifihost.LifecycleCommandEnable)
}

func TestRepeatedVoWiFiEnableWhileStartingAnswersPendingWithoutNewStart(t *testing.T) {
	r, lifecycle, _ := newVoWiFiLifecycleTestServer(t)
	startBackgroundedEnable(t, r, lifecycle)

	// A second start would queue behind the running one for the whole wait.
	vowifiStartWaitLimit = time.Minute
	rec := serveWithDeadline(t, r, http.MethodPatch, testVoWiFiPath, `{"enabled":true}`)
	requirePendingOK(t, rec)
	if !strings.Contains(rec.Body.String(), "VoWiFi 正在连接，结果请看设备概览") {
		t.Fatalf("body=%s, want the pending message", rec.Body.String())
	}

	disableAndRequireCanceled(t, r, lifecycle)
	if n := len(lifecycle.started); n != 0 {
		t.Fatalf("repeated enable started %d more lifecycle runs, want 0", n)
	}
}

func TestVoWiFiEnableWhileAnotherStartRunsSavesTheEnableIntent(t *testing.T) {
	const iccid = "8944000000000000123"
	r, lifecycle, pool := newVoWiFiLifecycleTestServer(t)
	// A start the user did not ask for holds the start while VoWiFi is off.
	requirePendingOK(t, serveWithDeadline(t, r, http.MethodPost, "/api/devices/dev-epdg/vowifi/actions/reconnect", ""))
	waitLifecycleKind(t, lifecycle.started, "start", vowifihost.LifecycleCommandRestart)
	pool.SetWorkerVoWiFiPolicy("dev-epdg", false)
	if err := db.UpsertCardPolicy(db.CardPolicy{ICCID: iccid, VoWiFiEnabled: false, Source: "user"}); err != nil {
		t.Fatal(err)
	}
	mode, strategy := pool.GetWorker("dev-epdg").Config.PhoneMode, pool.GetWorker("dev-epdg").Config.DataStrategy

	// A second start would queue behind the running one for the whole wait.
	vowifiStartWaitLimit = time.Minute
	requirePendingOK(t, serveWithDeadline(t, r, http.MethodPatch, testVoWiFiPath, `{"enabled":true}`))
	if got, err := db.GetCardPolicy(iccid); err != nil || !got.VoWiFiEnabled {
		t.Fatalf("card policy=%+v err=%v, want VoWiFi enabled", got, err)
	}
	if !pool.GetWorker("dev-epdg").Config.VoWiFiEnabled {
		t.Fatal("worker VoWiFi policy still off")
	}
	// The running start reads the call mode and data strategy without a lock.
	if got := pool.GetWorker("dev-epdg").Config; got.PhoneMode != mode || got.DataStrategy != strategy {
		t.Fatalf("worker mode=%q strategy=%q, want %q %q kept for the running start", got.PhoneMode, got.DataStrategy, mode, strategy)
	}
	if n := len(lifecycle.started); n != 0 {
		t.Fatalf("enable started %d more lifecycle runs, want 0", n)
	}

	rec := serveWithDeadline(t, r, http.MethodPatch, testVoWiFiPath, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	waitLifecycleKind(t, lifecycle.canceled, "cancel", vowifihost.LifecycleCommandRestart)
}

func TestChangesWhileVoWiFiStartingAnswerConflictWithoutStateChange(t *testing.T) {
	const iccid = "8944000000000000123"
	for _, tc := range []struct{ name, target, body string }{
		{name: "phone mode", target: testVoWiFiPath, body: `{"enabled":true,"mode":"cellular"}`},
		{name: "data strategy", target: testVoWiFiPath, body: `{"enabled":true,"data_strategy":"always"}`},
		{name: "network enable", target: "/api/devices/dev-epdg/network", body: `{"enabled":true}`},
		{name: "network disable", target: "/api/devices/dev-epdg/network", body: `{"enabled":false}`},
		{name: "flight mode on", target: "/api/devices/dev-epdg/flight-mode", body: `{"enabled":true}`},
		{name: "flight mode off", target: "/api/devices/dev-epdg/flight-mode", body: `{"enabled":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, lifecycle, pool := newVoWiFiLifecycleTestServer(t)
			startBackgroundedEnable(t, r, lifecycle)
			policy, err := db.GetCardPolicy(iccid)
			if err != nil {
				t.Fatalf("card policy after enable: %v", err)
			}
			cfg := pool.GetWorker("dev-epdg").Config

			rec := serveWithDeadline(t, r, http.MethodPatch, tc.target, tc.body)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "VoWiFi 正在连接，请先关闭或等待") {
				t.Fatalf("status=%d body=%s, want 409 while VoWiFi is starting", rec.Code, rec.Body.String())
			}
			if got, err := db.GetCardPolicy(iccid); err != nil || !reflect.DeepEqual(got, policy) {
				t.Fatalf("card policy changed: %+v err=%v, want %+v", got, err, policy)
			}
			if got := pool.GetWorker("dev-epdg").Config; !reflect.DeepEqual(got, cfg) {
				t.Fatalf("worker config changed: %+v, want %+v", got, cfg)
			}
			if n := len(lifecycle.canceled); n != 0 {
				t.Fatalf("rejected change canceled %d lifecycle runs", n)
			}
			// The 409 is written synchronously, so a cancel caused by this request
			// has already cleared the start claim.
			if !pool.IsVoWiFiStarting("dev-epdg") {
				t.Fatal("rejected change ended the running start")
			}

			disableAndRequireCanceled(t, r, lifecycle)
		})
	}
}

// An enable that omits the mode resolves it from the saved card policy. While a
// start runs, a saved mode other than the running one is a change too.
func TestVoWiFiEnableWithoutModeWhileStartingRejectsADifferentSavedMode(t *testing.T) {
	const iccid = "8944000000000000123"
	r, lifecycle, pool := newVoWiFiLifecycleTestServer(t)
	startBackgroundedEnable(t, r, lifecycle)
	policy, err := db.GetCardPolicy(iccid)
	if err != nil {
		t.Fatalf("card policy after enable: %v", err)
	}
	policy.PhoneMode = device.PhoneModeCellular
	if err := db.UpsertCardPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if policy, err = db.GetCardPolicy(iccid); err != nil {
		t.Fatal(err)
	}
	cfg := pool.GetWorker("dev-epdg").Config

	rec := serveWithDeadline(t, r, http.MethodPatch, testVoWiFiPath, `{"enabled":true}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "VoWiFi 正在连接，请先关闭或等待") {
		t.Fatalf("status=%d body=%s, want 409 while VoWiFi is starting", rec.Code, rec.Body.String())
	}
	if got, err := db.GetCardPolicy(iccid); err != nil || !reflect.DeepEqual(got, policy) {
		t.Fatalf("card policy changed: %+v err=%v, want %+v", got, err, policy)
	}
	if got := pool.GetWorker("dev-epdg").Config; !reflect.DeepEqual(got, cfg) {
		t.Fatalf("worker config changed: %+v, want %+v", got, cfg)
	}
	if n := len(lifecycle.canceled); n != 0 {
		t.Fatalf("rejected enable canceled %d lifecycle runs", n)
	}
	// Invalidating the runtime clears the start claim at once; the canceled
	// channel above only fills after the run wakes up.
	if !pool.IsVoWiFiStarting("dev-epdg") {
		t.Fatal("rejected enable ended the running start")
	}

	disableAndRequireCanceled(t, r, lifecycle)
}

// A Lebara UK card under its RF lock runs WiFi while cellular stays saved; a
// repeated enable without a mode matches that start and keeps the saved mode.
func TestVoWiFiEnableWithoutModeWhileStartingAcceptsTheLebaraProjection(t *testing.T) {
	const iccid = "8944000000000000123"
	r, lifecycle, pool := newVoWiFiLifecycleTestServer(t)
	startBackgroundedEnable(t, r, lifecycle)
	w := pool.GetWorker("dev-epdg")
	setNestedPrivateField(t, w, []string{"state", "Identity", "IMSI"}, "234870000000001")
	policy, err := db.GetCardPolicy(iccid)
	if err != nil {
		t.Fatalf("card policy after enable: %v", err)
	}
	policy.PhoneMode = device.PhoneModeCellular
	if err := db.UpsertCardPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if policy, err = db.GetCardPolicy(iccid); err != nil {
		t.Fatal(err)
	}
	w.Config.PhoneMode = device.PhoneModeWiFi
	w.Config.DataStrategy = policy.DataStrategy
	cfg := w.Config

	// A second start would queue behind the running one for the whole wait.
	vowifiStartWaitLimit = time.Minute
	requirePendingOK(t, serveWithDeadline(t, r, http.MethodPatch, testVoWiFiPath, `{"enabled":true}`))
	if got, err := db.GetCardPolicy(iccid); err != nil || !reflect.DeepEqual(got, policy) {
		t.Fatalf("card policy changed: %+v err=%v, want %+v", got, err, policy)
	}
	if got := pool.GetWorker("dev-epdg").Config; !reflect.DeepEqual(got, cfg) {
		t.Fatalf("worker config changed: %+v, want %+v", got, cfg)
	}
	if n := len(lifecycle.started); n != 0 {
		t.Fatalf("repeated enable started %d more lifecycle runs, want 0", n)
	}

	disableAndRequireCanceled(t, r, lifecycle)
}

// An explicit mode or data strategy equal to the running start is no change for
// the start, but it is a change of the saved card policy: a repeated enable
// saves it as main would, still without a new start or a worker write.
func TestVoWiFiEnableWithExplicitSettingWhileStartingSavesIt(t *testing.T) {
	const iccid = "8944000000000000123"
	for _, tc := range []struct {
		name, body string
		saved      func(*db.CardPolicy)
		want       func(*db.CardPolicy)
	}{
		{
			name:  "mode",
			body:  `{"enabled":true,"mode":"wifi"}`,
			saved: func(p *db.CardPolicy) { p.PhoneMode = device.PhoneModeCellular },
			want:  func(p *db.CardPolicy) { p.PhoneMode = device.PhoneModeWiFi },
		},
		{
			name:  "data strategy",
			body:  `{"enabled":true,"data_strategy":"on_demand"}`,
			saved: func(p *db.CardPolicy) { p.DataStrategy = "always" },
			want:  func(p *db.CardPolicy) { p.DataStrategy = "on_demand" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, lifecycle, pool := newVoWiFiLifecycleTestServer(t)
			startBackgroundedEnable(t, r, lifecycle)
			w := pool.GetWorker("dev-epdg")
			// Lebara keeps the running start on WiFi while cellular is saved.
			setNestedPrivateField(t, w, []string{"state", "Identity", "IMSI"}, "234870000000001")
			policy, err := db.GetCardPolicy(iccid)
			if err != nil {
				t.Fatalf("card policy after enable: %v", err)
			}
			tc.saved(&policy)
			if err := db.UpsertCardPolicy(policy); err != nil {
				t.Fatal(err)
			}
			if policy, err = db.GetCardPolicy(iccid); err != nil {
				t.Fatal(err)
			}
			w.Config.PhoneMode = device.PhoneModeWiFi
			w.Config.DataStrategy = "on_demand"
			// A value the worker setter would rewrite, so a worker write shows.
			w.Config.NetworkEnabled = true
			cfg := w.Config

			// A second start would queue behind the running one for the whole wait.
			limit := vowifiStartWaitLimit
			vowifiStartWaitLimit = time.Minute
			t.Cleanup(func() { vowifiStartWaitLimit = limit })
			requirePendingOK(t, serveWithDeadline(t, r, http.MethodPatch, testVoWiFiPath, tc.body))
			got, err := db.GetCardPolicy(iccid)
			if err != nil {
				t.Fatal(err)
			}
			want := policy
			tc.want(&want)
			want.UpdatedAt = got.UpdatedAt
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("card policy=%+v, want only the named setting changed: %+v", got, want)
			}
			if got := pool.GetWorker("dev-epdg").Config; !reflect.DeepEqual(got, cfg) {
				t.Fatalf("worker config changed: %+v, want %+v", got, cfg)
			}
			if n := len(lifecycle.started); n != 0 {
				t.Fatalf("repeated enable started %d more lifecycle runs, want 0", n)
			}
			if n := len(lifecycle.canceled); n != 0 || !pool.IsVoWiFiStarting("dev-epdg") {
				t.Fatalf("repeated enable canceled %d runs, starting=%v; want the running start kept", n, pool.IsVoWiFiStarting("dev-epdg"))
			}

			disableAndRequireCanceled(t, r, lifecycle)
		})
	}
}

// A plain repeated enable writes nothing, even when the card holds flight and
// data on their own that differ from what an enable would project.
func TestVoWiFiEnableRepeatWhileStartingKeepsIndependentSettings(t *testing.T) {
	const iccid = "8944000000000000123"
	r, lifecycle, pool := newVoWiFiLifecycleTestServer(t)
	startBackgroundedEnable(t, r, lifecycle)
	policy, err := db.GetCardPolicy(iccid)
	if err != nil {
		t.Fatalf("card policy after enable: %v", err)
	}
	policy.AirplaneEnabled = false
	policy.NetworkEnabled = true
	if err := db.UpsertCardPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if policy, err = db.GetCardPolicy(iccid); err != nil {
		t.Fatal(err)
	}
	cfg := pool.GetWorker("dev-epdg").Config

	limit := vowifiStartWaitLimit
	vowifiStartWaitLimit = time.Minute
	t.Cleanup(func() { vowifiStartWaitLimit = limit })
	requirePendingOK(t, serveWithDeadline(t, r, http.MethodPatch, testVoWiFiPath, `{"enabled":true}`))
	if got, err := db.GetCardPolicy(iccid); err != nil || !reflect.DeepEqual(got, policy) {
		t.Fatalf("card policy changed: %+v err=%v, want %+v", got, err, policy)
	}
	if got := pool.GetWorker("dev-epdg").Config; !reflect.DeepEqual(got, cfg) {
		t.Fatalf("worker config changed: %+v, want %+v", got, cfg)
	}
	if n := len(lifecycle.started); n != 0 {
		t.Fatalf("repeated enable started %d more lifecycle runs, want 0", n)
	}

	disableAndRequireCanceled(t, r, lifecycle)
}

type cardRestartCall struct {
	step, iccid string
	reconnect   bool
}

// blockingCardRestarter stands in for card-policy restarts whose VoWiFi start
// keeps retrying until released. It reports each call on started.
type blockingCardRestarter struct {
	release          chan struct{}
	releaseOnce      sync.Once
	started          chan cardRestartCall
	proxyErr, mwiErr error
}

func newBlockingCardRestarter(t *testing.T) *blockingCardRestarter {
	b := &blockingCardRestarter{release: make(chan struct{}), started: make(chan cardRestartCall, 8)}
	t.Cleanup(b.releaseAll)
	return b
}

func (b *blockingCardRestarter) releaseAll() { b.releaseOnce.Do(func() { close(b.release) }) }

func (b *blockingCardRestarter) RestartVoWiFiForICCID(iccid string) error {
	b.started <- cardRestartCall{step: "proxy_restart", iccid: iccid}
	<-b.release
	return b.proxyErr
}

func (b *blockingCardRestarter) RedetectMWIForICCID(iccid string, reconnect bool) error {
	b.started <- cardRestartCall{step: "mwi_redetect", iccid: iccid, reconnect: reconnect}
	<-b.release
	return b.mwiErr
}

func (b *blockingCardRestarter) waitCall(t *testing.T, want cardRestartCall) {
	t.Helper()
	select {
	case got := <-b.started:
		if got != want {
			t.Fatalf("call=%+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was not called", want.step)
	}
}

// newCardPolicyRestartTestServer serves PUT /api/cards/:iccid/policy for card
// 8944101, held by dev-card-a and dev-card-b, with restarts handled by b.
func newCardPolicyRestartTestServer(t *testing.T, policy db.CardPolicy, remembered bool, b *blockingCardRestarter) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	shortenVoWiFiStartWait(t)
	openTestDB(t)
	now := time.Now()
	if err := db.UpsertUpstreamProxy(db.UpstreamProxy{
		ID: "uk-a", Addr: "127.0.0.1:1080", Enabled: true, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	pool := device.NewPool(&config.Config{})
	t.Cleanup(func() { _ = pool.Shutdown() })
	for _, id := range []string{"dev-card-b", "dev-card-a"} {
		worker := &device.Worker{ID: id, Config: config.DeviceConfig{ID: id}}
		setNestedPrivateField(t, worker, []string{"state", "Identity", "ICCID"}, "8944101F")
		injectWorker(pool, worker)
	}
	store := &cardPolicyStoreStub{policy: policy, remembered: remembered}
	s := &Server{pool: pool, cardPolicies: store, cardPolicyRestarter: b, cardPolicyMWIRedetector: b}
	r := gin.New()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)
	return r
}

func requireSavedCardPolicy(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var got db.CardPolicy
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ICCID != "8944101" {
		t.Fatalf("body=%s err=%v, want the saved policy", rec.Body.String(), err)
	}
}

func TestPutCardPolicyReturnsSavedPolicyWhileVoWiFiRestartRetries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		policy     db.CardPolicy
		remembered bool
		call       cardRestartCall
	}{
		{name: "proxy change", body: `{"vowifi_upstream_proxy_id":"uk-a"}`,
			policy: db.CardPolicy{ICCID: "8944101", VoWiFiEnabled: true, Source: "user"},
			call:   cardRestartCall{step: "proxy_restart", iccid: "8944101"}},
		{name: "mwi redetect", body: `{"mwi_mode":"on"}`, remembered: true,
			policy: db.CardPolicy{ICCID: "8944101", MWIMode: "auto", VoWiFiEnabled: true, Source: "user"},
			call:   cardRestartCall{step: "mwi_redetect", iccid: "8944101", reconnect: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocked := newBlockingCardRestarter(t)
			r := newCardPolicyRestartTestServer(t, tc.policy, tc.remembered, blocked)

			requireSavedCardPolicy(t, serveWithDeadline(t, r, http.MethodPut, "/api/cards/8944101/policy", tc.body))
			blocked.waitCall(t, tc.call)
			if n := len(blocked.started); n != 0 {
				t.Fatalf("restart calls=%d more, want 1 in total", n)
			}
		})
	}
}

func TestPutCardPolicyRunsMWIRedetectAfterPendingProxyRestart(t *testing.T) {
	logs := captureStartWaitLogs(t)
	blocked := newBlockingCardRestarter(t)
	r := newCardPolicyRestartTestServer(t,
		db.CardPolicy{ICCID: "8944101", MWIMode: "auto", VoWiFiEnabled: true, Source: "user"}, true, blocked)

	requireSavedCardPolicy(t, serveWithDeadline(t, r, http.MethodPut, "/api/cards/8944101/policy",
		`{"vowifi_upstream_proxy_id":"uk-a","mwi_mode":"on"}`))
	blocked.waitCall(t, cardRestartCall{step: "proxy_restart", iccid: "8944101"})
	if n := len(blocked.started); n != 0 {
		t.Fatalf("%d calls started before the proxy restart finished, want MWI to wait", n)
	}
	fields := waitLog(t, logs, "warn", "[dev-card-a,dev-card-b] VoWiFi 启动未在限时内完成，释放设备锁后在后台继续")
	if fields["op"] != "card_policy_proxy_restart+mwi_redetect" {
		t.Fatalf("timeout fields=%v, want the combined op", fields)
	}

	blocked.releaseAll()
	blocked.waitCall(t, cardRestartCall{step: "mwi_redetect", iccid: "8944101"})
	waitBackgroundStarts(t)
	waitLog(t, logs, "info", "[dev-card-a,dev-card-b] 后台 VoWiFi 启动完成")
}

func TestPutCardPolicyReportsTheFailedRestartStep(t *testing.T) {
	for _, tc := range []struct {
		name             string
		proxyErr, mwiErr error
		calls            []cardRestartCall
		step             string
	}{
		{name: "proxy restart fails", proxyErr: errors.New("ike auth failed"), step: "proxy_changed",
			calls: []cardRestartCall{{step: "proxy_restart", iccid: "8944101"}}},
		{name: "mwi redetect fails", mwiErr: errors.New("mwi store unavailable"), step: "redetect_mwi",
			calls: []cardRestartCall{{step: "proxy_restart", iccid: "8944101"}, {step: "mwi_redetect", iccid: "8944101"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureStartWaitLogs(t)
			blocked := newBlockingCardRestarter(t)
			blocked.proxyErr, blocked.mwiErr = tc.proxyErr, tc.mwiErr
			blocked.releaseAll()
			r := newCardPolicyRestartTestServer(t,
				db.CardPolicy{ICCID: "8944101", MWIMode: "auto", VoWiFiEnabled: true, Source: "user"}, true, blocked)

			rec := serveWithDeadline(t, r, http.MethodPut, "/api/cards/8944101/policy",
				`{"vowifi_upstream_proxy_id":"uk-a","mwi_mode":"on"}`)
			want := errors.Join(tc.proxyErr, tc.mwiErr).Error()
			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), cardPolicySavedRestartFailedCode) ||
				!strings.Contains(rec.Body.String(), want) {
				t.Fatalf("code=%d body=%s, want the saved-restart-failed error with %q", rec.Code, rec.Body.String(), want)
			}
			for _, call := range tc.calls {
				blocked.waitCall(t, call)
			}
			if n := len(blocked.started); n != 0 {
				t.Fatalf("%d more calls after the failed step", n)
			}
			fields := waitLog(t, logs, "warn", "卡策略已保存，但 WiFi calling 重连失败")
			other := map[string]string{"proxy_changed": "redetect_mwi", "redetect_mwi": "proxy_changed"}[tc.step]
			if fields[tc.step] != true || fields[other] != nil {
				t.Fatalf("failure fields=%v, want only the %s step", fields, tc.step)
			}
		})
	}
}

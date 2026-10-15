package device

import (
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
)

func replaceWorkerForLifecycleTest(p *Pool, worker *Worker) {
	p.assignWorkerGeneration(worker)
	p.mu.Lock()
	p.workers[worker.ID] = worker
	p.mu.Unlock()
}

func TestAbandonDeviceCancelsWorkerLifecycleTimers(t *testing.T) {
	p, port := testModemVoicePort(t)
	deviceID := port.worker.ID
	owner := p.captureWorkerLifecycleOwner(deviceID)
	simTimer := time.AfterFunc(time.Hour, func() {})
	wakeupTimer := time.AfterFunc(time.Hour, func() {})
	t.Cleanup(func() {
		simTimer.Stop()
		wakeupTimer.Stop()
	})

	p.simEventTimers[deviceID] = simTimer
	p.deviceEventWakeups[deviceID] = &deviceEventRecoverWakeup{
		timer: wakeupTimer, owner: owner, sources: map[string]struct{}{"test": {}},
	}
	if err := p.AbandonDevice(deviceID); err != nil {
		t.Fatal(err)
	}
	if p.simEventTimers[deviceID] != nil || p.deviceEventWakeups[deviceID] != nil {
		t.Fatal("abandoned device retained delayed lifecycle callbacks")
	}
}

func TestStaleDeviceEventCannotFlushReplacementWakeup(t *testing.T) {
	p := NewPool(&config.Config{})
	old := &Worker{ID: "dev"}
	p.AttachWorkerForTest(old)
	stale := &deviceEventRecoverWakeup{
		owner: p.captureWorkerLifecycleOwner(old.ID), sources: map[string]struct{}{"old": {}},
	}
	replacement := &Worker{ID: old.ID}
	replaceWorkerForLifecycleTest(p, replacement)
	current := &deviceEventRecoverWakeup{
		owner: p.captureWorkerLifecycleOwner(replacement.ID), sources: map[string]struct{}{"new": {}},
	}
	p.deviceEventWakeups[old.ID] = current

	p.flushDesiredVoWiFiRecoverFromDeviceEvent(old.ID, stale)
	if p.deviceEventWakeups[old.ID] != current {
		t.Fatal("stale device event consumed the replacement wakeup")
	}
}

func TestVoWiFiReadyStartRejectsReplacementWorker(t *testing.T) {
	p := NewPool(&config.Config{})
	old := &Worker{ID: "dev"}
	p.AttachWorkerForTest(old)
	start := p.captureVoWiFiReadyStart(old)

	replacement := &Worker{ID: old.ID}
	replaceWorkerForLifecycleTest(p, replacement)
	p.voWiFiHost().InvalidateRuntime(old.ID, "worker_replaced")
	if err := p.enableVoWiFiWhenReady(start, time.Millisecond, "test"); err == nil {
		t.Fatal("stale automatic VoWiFi start accepted the replacement worker")
	}
}

func TestOnDemandCallCleanupRejectsReplacementWorker(t *testing.T) {
	p := NewPool(&config.Config{})
	network := &fakeController{connected: true}
	old := &Worker{ID: "dev", netOverride: network}
	p.AttachWorkerForTest(old)
	checkOwner := p.outboundOwnerCheck(old)
	replaceWorkerForLifecycleTest(p, &Worker{ID: old.ID})

	p.cleanupOnDemandCellularCall(old, checkOwner)
	if !network.connected {
		t.Fatal("old call cleanup disconnected the replaced worker resources")
	}
}

func TestRetiredWorkerCannotReclaimPersistedSIM(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	p := NewPool(&config.Config{})
	old := &Worker{ID: "dev", Pool: p, Config: config.DeviceConfig{ModemIMEI: "imei"}}
	old.state.Identity.IMEI = "imei"
	setWorkerSMSIdentity(old, SMSIdentity{ICCID: "card-old", IMSI: "imsi-old"})
	p.AttachWorkerForTest(old)
	p.PersistRuntimeState(old)
	p.PersistIdentityState(old)

	replacement := &Worker{ID: old.ID, Pool: p, Config: old.Config}
	replacement.state.Identity.IMEI = "imei"
	setWorkerSMSIdentity(replacement, SMSIdentity{ICCID: "card-new", IMSI: "imsi-new"})
	replaceWorkerForLifecycleTest(p, replacement)
	p.PersistIdentityState(replacement)
	p.PersistIdentityState(old)

	if got := db.CurrentICCIDForDevice(old.ID); got != "card-new" {
		t.Fatalf("retired worker restored ICCID %q", got)
	}
}

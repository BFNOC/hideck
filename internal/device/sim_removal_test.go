package device

import (
	"testing"

	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
)

func TestPhysicalSIMInsertEventRefreshesIdentity(t *testing.T) {
	p := NewPool(&config.Config{})
	t.Cleanup(func() { _ = p.Shutdown() })
	deviceBackend := &workerStartupIdentityBackendStub{liveICCID: "card-new", liveIMSI: "imsi-new"}
	deviceBackend.simInserted = true
	w := &Worker{ID: "dev", Pool: p, Backend: deviceBackend}
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-old", IMSI: "imsi-old"})
	p.AttachWorkerForTest(w)

	p.confirmSIMRemovedAndStopVoWiFi(w.ID, "test")
	if got := w.CurrentICCID(); got != "card-new" {
		t.Fatalf("ICCID=%q want card-new", got)
	}
	if got := w.GetCachedIMSI(); got != "imsi-new" {
		t.Fatalf("IMSI=%q want imsi-new", got)
	}
	p.deviceEventWakeMu.Lock()
	_, recoveryScheduled := p.deviceEventWakeups[w.ID]
	p.deviceEventWakeMu.Unlock()
	if !recoveryScheduled {
		t.Fatal("refreshed SIM did not schedule desired VoWiFi recovery")
	}
}

func TestConfirmedSIMRemovalClearsIdentityAndDatabaseBinding(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	p := NewPool(&config.Config{})
	t.Cleanup(func() { _ = p.Shutdown() })
	const imei = "860000000000333"
	if err := db.UpsertDevice(imei, "dev", "model", "port"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSIMCardIdentity("card-old", "imsi-old", "", ptrString(imei)); err != nil {
		t.Fatal(err)
	}
	deviceBackend := &workerStartupIdentityBackendStub{}
	deviceBackend.simInserted = false
	w := &Worker{ID: "dev", Pool: p, Backend: deviceBackend, Config: config.DeviceConfig{ModemIMEI: imei}}
	w.state.Identity.IMEI = imei
	setWorkerSMSIdentity(w, SMSIdentity{ICCID: "card-old", IMSI: "imsi-old"})
	p.AttachWorkerForTest(w)
	before := w.SIMIdentityGeneration()

	p.confirmSIMRemovedAndStopVoWiFi(w.ID, "test")
	if got := w.CurrentICCID(); got != "" {
		t.Fatalf("runtime retained removed SIM %q", got)
	}
	if w.SIMIdentityGeneration() <= before {
		t.Fatal("SIM removal did not retire the previous identity generation")
	}
	if got := db.CurrentICCIDForDevice(w.ID); got != "" {
		t.Fatalf("database retained removed SIM %q", got)
	}
}

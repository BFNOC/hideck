package device

import "testing"

type phoneWorkerFixture struct {
	worker *Worker
	iccid  string
}

func testPhoneWorker(t *testing.T) (*Pool, *phoneWorkerFixture) {
	t.Helper()
	p, w := atTestPool(t)
	w.Config.PhoneMode, w.Config.VoWiFiEnabled = PhoneModeCellular, true
	w.state.Identity.ICCID, w.state.Identity.IMSI = "test-sim", "234150000000001"
	return p, &phoneWorkerFixture{worker: w, iccid: "test-sim"}
}

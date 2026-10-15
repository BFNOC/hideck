package identity

import "testing"

func TestPrepareStartKeepsICCID(t *testing.T) {
	access := &stubAccess{ident: Identity{
		IMSI: "310280233621715", IMPI: "310280233621715@private.att.net",
		IMPU:   []string{"sip:310280233621715@one.att.net"},
		Domain: "one.att.net",
	}}
	prepared, err := PrepareStart(PrepareStartInput{
		DeviceID: "wwan0",
		Profile:  Profile{ICCID: " 89012802331234567890 ", IMSI: "310280233621715", MCC: "310", MNC: "280"},
		Access:   access,
	})
	if err != nil {
		t.Fatalf("PrepareStart: %v", err)
	}
	if prepared.Profile.ICCID != "89012802331234567890" {
		t.Fatalf("prepared ICCID = %q", prepared.Profile.ICCID)
	}
}

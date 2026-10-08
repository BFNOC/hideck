package imscore

import (
	"strings"
	"testing"

	"github.com/iniwex5/vowifi-go/internal/vowifi/imsheaders"
)

func TestSIPInstanceFallsBackToStableUUIDWithoutIMEI(t *testing.T) {
	first := &IMSConfig{IMSI: "234870000000001"}
	first.syncCompatibilityFields()
	again := &IMSConfig{IMSI: "234870000000001"}
	again.syncCompatibilityFields()
	other := &IMSConfig{IMSI: "234870000000002"}
	other.syncCompatibilityFields()
	if !strings.HasPrefix(first.SIPInstance, "<urn:uuid:") || first.SIPInstance != again.SIPInstance ||
		first.SIPInstance == other.SIPInstance || first.IMEI != first.SIPInstance {
		t.Fatalf("instances = %q %q %q, IMEI %q", first.SIPInstance, again.SIPInstance, other.SIPInstance, first.IMEI)
	}
	params := imsheaders.ContactParams(imsheaders.ContactOptions{
		SIPInstance: first.IMEI, ContactParamOrder: []string{"sip_instance", "reg_id"},
	})
	if len(params) != 2 || params[0].Value != `"`+first.SIPInstance+`"` {
		t.Fatalf("contact params = %+v", params)
	}

	withIMEI := &IMSConfig{IMSI: "234870000000001", IMEI: "358983361433761"}
	withIMEI.syncCompatibilityFields()
	if withIMEI.SIPInstance != "358983361433761" {
		t.Fatalf("IMEI instance = %q", withIMEI.SIPInstance)
	}
}

func TestSIPInstanceKind(t *testing.T) {
	for contact, want := range map[string]string{
		`<sip:a@b>;+sip.instance="<urn:gsma:imei:35898336-143376-1>";reg-id=1`:      "imei",
		`<sip:a@b>;+sip.instance="<urn:uuid:0d3e6a2b-0000-5000-8000-000000000000>"`: "uuid",
		`<sip:a@b>;+sip.instance="";reg-id=1`:                                       "empty",
		`<sip:a@b>;expires=600`:                                                     "none",
	} {
		if got := sipInstanceKind(contact); got != want {
			t.Errorf("sipInstanceKind(%q) = %q, want %q", contact, got, want)
		}
	}
}

package imscore

import (
	"strings"
	"testing"
)

func TestInboundResponseCopiesEveryViaInOrder(t *testing.T) {
	request := "NOTIFY sip:ue@[2001:db8::10]:40696;transport=tcp SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP [2001:db8::5]:6000;branch=z9hG4bK-pcscf\r\n" +
		"Via: SIP/2.0/TCP scscf.ims.example;branch=z9hG4bK-scscf, SIP/2.0/UDP as.ims.example;branch=z9hG4bK-as\r\n" +
		"From: <sip:user@ims.example>;tag=remote\r\n" +
		"To: <sip:user@ims.example>;tag=local\r\n" +
		"Call-ID: call-n\r\n" +
		"CSeq: 1 NOTIFY\r\n" +
		"Event: reg\r\n" +
		"Content-Length: 0\r\n\r\n"
	response, err := buildSIPRequestResponse(request, 200)
	if err != nil {
		t.Fatal(err)
	}
	var vias []string
	for _, line := range strings.Split(response, "\r\n") {
		if value, ok := strings.CutPrefix(line, "Via: "); ok {
			vias = append(vias, value)
		}
	}
	want := []string{"[2001:db8::5]:6000;branch=z9hG4bK-pcscf", "scscf.ims.example;branch=z9hG4bK-scscf", "as.ims.example;branch=z9hG4bK-as"}
	if len(vias) != len(want) {
		t.Fatalf("Via lines = %q, want %d in order", vias, len(want))
	}
	for i, sentBy := range want {
		if !strings.Contains(vias[i], sentBy) {
			t.Fatalf("Via %d = %q, want %q", i, vias[i], sentBy)
		}
	}
}

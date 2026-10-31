package imscore

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

func TestContainsHeaderToken(t *testing.T) {
	if !containsHeaderToken("path, sec-agree, outbound", "outbound") {
		t.Fatal("outbound token was not detected")
	}
	if containsHeaderToken("path, outbound-flow", "outbound") {
		t.Fatal("partial outbound token was accepted")
	}
}

func TestContainsSIPParameter(t *testing.T) {
	path := `<sip:edge.example;lr;ob>, <sip:pcscf.example;lr>`
	if !containsSIPParameter(path, "ob") {
		t.Fatal("Path ob parameter was not detected")
	}
	contact := `<sip:user@example>;reg-id=1;+sip.instance="<urn:uuid:test>"`
	if !containsSIPParameter(contact, "reg-id") {
		t.Fatal("Contact reg-id parameter was not detected")
	}
	if containsSIPParameter(contact, "id") {
		t.Fatal("partial Contact parameter was accepted")
	}
}

func TestSupportedOutboundDoesNotRequireBindingRefresh(t *testing.T) {
	service := &Service{}
	service.logRegisterFlowNegotiation(&sipResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Supported": "path, sec-agree, outbound"},
	})
	if service.sipOutbound {
		t.Fatal("Supported: outbound alone must not enable outbound Contact params")
	}
	if service.needsOutboundBindingRefresh() {
		t.Fatal("Supported: outbound alone must not trigger a follow-up REGISTER")
	}
}

func TestRegisterFlowNegotiationLogsAuthenticationInfo(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	cases := []struct {
		name       string
		headers    map[string]string
		authInfo   bool
		nextnonce  bool
		flowTimerS float64
	}{
		{name: "absent", headers: map[string]string{}},
		{name: "rspauth only", headers: map[string]string{"Authentication-Info": `qop=auth, rspauth="6629fae4", nc=00000001`}, authInfo: true},
		{name: "nextnonce", headers: map[string]string{"Authentication-Info": `nextnonce="bmV4dA==", qop=auth`, "Flow-Timer": "120"}, authInfo: true, nextnonce: true, flowTimerS: 120},
	}
	for _, tc := range cases {
		output.Reset()
		(&Service{}).logRegisterFlowNegotiation(&sipResponse{StatusCode: 200, Headers: tc.headers})
		var entry map[string]any
		if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
			t.Fatalf("%s: decode log %q: %v", tc.name, output.String(), err)
		}
		if entry["authentication_info"] != tc.authInfo || entry["nextnonce"] != tc.nextnonce ||
			entry["flow_timer"] != tc.flowTimerS*float64(time.Second) {
			t.Fatalf("%s: log = %v", tc.name, entry)
		}
	}
}

func TestRequiredOutboundRequiresBindingRefresh(t *testing.T) {
	service := &Service{}
	service.logRegisterFlowNegotiation(&sipResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Require": "outbound"},
	})
	if !service.needsOutboundBindingRefresh() {
		t.Fatal("Require: outbound must trigger a follow-up REGISTER")
	}
}

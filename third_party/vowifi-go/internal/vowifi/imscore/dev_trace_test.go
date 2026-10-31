package imscore

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestDevRedactSIPMasksIdentities(t *testing.T) {
	raw := "REGISTER sip:ims.mnc002.mcc262.3gppnetwork.org SIP/2.0\r\n" +
		"From: \"Alice Example\" <sip:262021234567897@ims.mnc002.mcc262.3gppnetwork.org>;tag=1\r\n" +
		"Contact: <sip:u@[2a00::1]:5060>;+sip.instance=\"<urn:gsma:imei:86034905-086973-4>\"\r\n" +
		"Path: <sip:[2a01::5]:6000;lr;imei:86034905-086973-4>\r\n" +
		"P-Associated-URI: <tel:+49-151-1234567>, <sip:alice.example@ims.example>\r\n" +
		"P-Charging-Vector: icid-value=\"x\";bidx=27123464\r\n" +
		"P-Access-Network-Info: IEEE-802.11; i-wlan-node-id=\"22c4efde5448\";country=DE\r\n" +
		"Authorization: Digest username=\"262021234567897@ims\", nonce=\"abc\",\r\n" +
		" response=\"def\"\r\n" +
		"Proxy-Authorization : Digest response=\"ghi\"\r\n" +
		"To: Bob Example <sip:bob;desk,1@example.org>\r\n" +
		"Security-Verify: ipsec-3gpp; alg=hmac-sha-1-96; spi-c=8232471; spi-s=8232470; port-c=32821; port-s=6000\r\n" +
		"\r\n<reginfo><display-name>Carol</display-name></reginfo>"
	redacted := devRedactSIP(raw)
	for _, secret := range []string{"262021", "97@", "086973", "151-1234567", "alice.example", "Alice Example",
		"271", "22c4efde5448", "response=\"def\"", "ghi", "Bob Example", "bob;desk", "Carol"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("%q survived redaction:\n%s", secret, redacted)
		}
	}
	for _, kept := range []string{"country=DE", "REGISTER sip:ims.mnc002.mcc262.3gppnetwork.org", "[2a00::1]:5060", "[2a01::5]:6000",
		"spi-c=8232471; spi-s=8232470; port-c=32821"} {
		if !strings.Contains(redacted, kept) {
			t.Fatalf("redaction removed %q:\n%s", kept, redacted)
		}
	}
}

func TestDevInboundMessageWithBareNewlinesLogsHeadersOnly(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	raw := "MESSAGE sip:u@[2a00::1]:5060 SIP/2.0\nCall-ID: m1\nContent-Length: 11\n\nsecret-body"
	(&Service{}).devLogInboundRequest(sip.NewRequest(sip.MESSAGE, sip.Uri{Host: "example"}), raw, nil)
	if strings.Contains(output.String(), "secret-body") {
		t.Fatalf("MESSAGE log = %s", output.String())
	}
}

func TestDevInboundMessageLogsHeadersOnly(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	raw := "MESSAGE sip:u@[2a00::1]:5060 SIP/2.0\r\nCall-ID: m1\r\nCSeq: 1 MESSAGE\r\n" +
		"Content-Type: application/vnd.3gpp.sms\r\nContent-Length: 11\r\n\r\nsecret-body"
	request := sip.NewRequest(sip.MESSAGE, sip.Uri{Host: "example"})
	(&Service{}).devLogInboundRequest(request, raw, nil)
	if strings.Contains(output.String(), "secret-body") || !strings.Contains(output.String(), "method=MESSAGE") {
		t.Fatalf("MESSAGE log = %s", output.String())
	}
}

func TestDevRegisterRequestDiffSeesMaskedChanges(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	service := &Service{}
	session := &registerSession{callID: "c1", cseq: 3}
	request := "REGISTER sip:ims SIP/2.0\r\nCSeq: 3 REGISTER\r\nAuthorization: Digest response=\"%s\"\r\n\r\n"
	service.devLogRegisterRequest(session, fmt.Sprintf(request, "first"))
	session.cseq = 4
	service.devLogRegisterRequest(session, fmt.Sprintf(request, "second"))
	if !strings.Contains(output.String(), "changed_headers=[authorization]") ||
		strings.Contains(output.String(), "first") || strings.Contains(output.String(), "second") {
		t.Fatalf("log = %s", output.String())
	}
}

func TestDevChangedSIPHeaders(t *testing.T) {
	before := "REGISTER sip:a SIP/2.0\r\nCSeq: 3 REGISTER\r\nVia: x;branch=1\r\nExpires: 600000\r\n" +
		"Security-Verify: a\r\nP-Access-Network-Info: x\r\n\r\n"
	after := "REGISTER sip:a SIP/2.0\r\nCSeq: 4 REGISTER\r\nVia: x;branch=2\r\nExpires: 600000\r\n" +
		"Security-Verify: b\r\nSupported: path\r\n\r\n"
	got := devChangedSIPHeaders(before, after)
	want := []string{"p-access-network-info(removed)", "security-verify", "supported"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed = %v, want %v", got, want)
	}
}

func TestDevSampled(t *testing.T) {
	for count, want := range map[uint64]bool{1: true, 2: false, 10: true, 11: false, 100: true, 1000: true, 20: false} {
		if got := devSampled(count); got != want {
			t.Fatalf("devSampled(%d) = %t, want %t", count, got, want)
		}
	}
}

func TestDevTriggerCoversEveryAttemptOfOneRegistration(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	service := &Service{}
	service.devRecordTrigger("port-s flow failed")
	service.devBeginRegister()
	session := &registerSession{refresh: true}
	// A transport fallback runs a second attempt in the same registration.
	service.devLogRegisterContext(session)
	service.devLogRegisterContext(session)
	if strings.Count(output.String(), `trigger="port-s flow failed"`) != 2 {
		t.Fatalf("log = %s", output.String())
	}
	output.Reset()
	service.devBeginRegister()
	service.devLogRegisterContext(session)
	if !strings.Contains(output.String(), "trigger=scheduled") {
		t.Fatalf("next registration log = %s", output.String())
	}
}

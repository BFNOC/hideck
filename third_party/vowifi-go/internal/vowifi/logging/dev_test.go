package logging

import (
	"strings"
	"testing"
)

func TestDevRedactSIPMasksIdentities(t *testing.T) {
	raw := "REGISTER sip:ims.mnc002.mcc262.3gppnetwork.org SIP/2.0\r\n" +
		"From: \"Alice Example\" <sip:262021234567897@ims.mnc002.mcc262.3gppnetwork.org>;tag=1\r\n" +
		"Contact: <sip:u@[2a00::1]:5060>;+g.3gpp.accesstype=\"wlan1\";+sip.instance=\"<urn:gsma:imei:86034905-086973-4>\"\r\n" +
		"Path: <sip:[2a01::5]:6000;lr;imei:86034905-086973-4>\r\n" +
		"P-Associated-URI: <tel:+49-151-1234567>, <sip:alice.example@ims.example>\r\n" +
		"P-Charging-Vector: icid-value=\"x\";bidx=27123464\r\n" +
		"P-Access-Network-Info: IEEE-802.11; i-wlan-node-id=\"22c4efde5448\";country=DE\r\n" +
		"Authorization: Digest username=\"262021234567897@ims\", nonce=\"abc\",\r\n" +
		" response=\"def\"\r\n" +
		"Proxy-Authorization : Digest response=\"ghi\"\r\n" +
		"WWW-Authenticate: Digest realm=\"ims.example\", nonce=\"cmFuZGF1dG4=\", algorithm=AKAv1-MD5\r\n" +
		"To: Bob Example <sip:bob;desk,1@example.org>\r\n" +
		"Security-Verify: ipsec-3gpp; alg=hmac-sha-1-96; spi-c=8232471; spi-s=8232470; port-c=32821; port-s=6000\r\n" +
		"Content-Type: application/reginfo+xml\r\n" +
		"\r\n<reginfo><display-name>Carol</display-name><uri>sip:+491511234567@ims</uri></reginfo>"
	redacted := DevRedactSIP(raw)
	for _, secret := range []string{"262021", "97@", "086973", "151-1234567", "alice.example", "Alice Example",
		"271", "22c4efde5448", "response=\"def\"", "ghi", "cmFuZGF1dG4", "Bob Example", "bob;desk", "Carol",
		"491511234567"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("%q survived redaction:\n%s", secret, redacted)
		}
	}
	for _, kept := range []string{"country=DE", `+g.3gpp.accesstype="wlan1";+sip.instance="<urn:gsma:imei:<masked>>"`, "REGISTER sip:ims.mnc002.mcc262.3gppnetwork.org", "[2a00::1]:5060",
		"[2a01::5]:6000", "spi-c=8232471; spi-s=8232470; port-c=32821", "realm=\"ims.example\"",
		"algorithm=AKAv1-MD5", "<reginfo>"} {
		if !strings.Contains(redacted, kept) {
			t.Fatalf("redaction removed %q:\n%s", kept, redacted)
		}
	}
}

func TestDevRedactSIPMasksAuthenticationInfoAndSDPKeys(t *testing.T) {
	raw := "SIP/2.0 200 OK\r\n" +
		"Authentication-Info: nextnonce=\"n3xt\", rspauth=\"r5p\", qop=auth\r\n" +
		"WWW-Authenticate: Digest realm=\"x\", nonce=\"abc\", ck=\"c0ffee\", ik=\"beef\"\r\n" +
		"Content-Type: application/sdp\r\n\r\n" +
		"v=0\r\nk=base64:secretkey\r\na=key-mgmt:mikey AQAFsecret\r\n" +
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:WVNfX19zZW1jdGwgKCkgewkyMjA7fQp9CnVubGVz|2^20|1:4\r\n" +
		"m=audio 49170 RTP/AVP 0\r\n"
	redacted := DevRedactSIP(raw)
	for _, secret := range []string{"n3xt", "r5p", "c0ffee", "beef", "secretkey", "AQAFsecret", "WVNfX19zZW1jdGwgKCkgewkyMjA7fQp9CnVubGVz"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("%q survived redaction:\n%s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, "m=audio 49170 RTP/AVP 0") || !strings.Contains(redacted, "AES_CM_128_HMAC_SHA1_80 inline:<masked>") {
		t.Fatalf("SDP lost too much:\n%s", redacted)
	}
}

func TestDevRedactSIPOmitsSMSBodies(t *testing.T) {
	for _, raw := range []string{
		"MESSAGE sip:u@[2a00::1]:5060 SIP/2.0\r\nCall-ID: m1\r\nContent-Type: application/vnd.3gpp.sms\r\n" +
			"Content-Length: 11\r\n\r\nsecret-body",
		"MESSAGE sip:u@[2a00::1]:5060 SIP/2.0\nCall-ID: m1\nContent-Length: 11\n\nsecret-body",
	} {
		redacted := DevRedactSIP(raw)
		if strings.Contains(redacted, "secret-body") || !strings.Contains(redacted, "<body omitted") ||
			!strings.Contains(redacted, "Call-ID: m1") {
			t.Fatalf("redacted = %q", redacted)
		}
	}
}

func TestDevSampled(t *testing.T) {
	for count, want := range map[uint64]bool{1: true, 2: false, 10: true, 11: false, 100: true, 1000: true, 20: false} {
		if got := DevSampled(count); got != want {
			t.Fatalf("DevSampled(%d) = %t, want %t", count, got, want)
		}
	}
}

func TestDevRedactTextMasksAStatusDump(t *testing.T) {
	raw := `{"impi":"262021234567897@ims.mnc002.mcc262.3gppnetwork.org","impu":"sip:+491511234567@ims.example",` +
		`"impus":["tel:+491511234567"],"associated_msisdn":"491511234567",` +
		`"path":"<sip:[2a01::5]:6000;lr;imei:86034905-086973-4>","registrar":"[2a01::5]:6000"}`
	redacted := DevRedactText(raw)
	for _, secret := range []string{"262021", "1234567", "086973"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("%q survived redaction: %s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, `"registrar":"[2a01::5]:6000"`) {
		t.Fatalf("registrar was masked: %s", redacted)
	}
}

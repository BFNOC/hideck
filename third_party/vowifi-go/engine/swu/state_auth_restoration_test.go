package swu

import (
	"bytes"
	"errors"
	"testing"

	"github.com/iniwex5/vowifi-go/engine/ikev2"
	"github.com/iniwex5/vowifi-go/engine/swu/eapaka"
)

func TestNormalizeAKAChallengeModeMatchesLegacyAliases(t *testing.T) {
	tests := map[string]string{
		"": "minimal", "minimal": "minimal",
		"off": "off", "NONE": "off", "omit": "off", "no_checkcode": "off",
		"echo": "checkcode", "checkcode": "checkcode",
		"recalc": "recompute", " ReCompute ": "recompute",
		"custom": "custom",
	}
	for input, want := range tests {
		if got := normalizeAKAChallengeMode(input); got != want {
			t.Errorf("normalizeAKAChallengeMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMinimalAKAChallengePreservesEmptyCheckcode(t *testing.T) {
	session := NewSession(&Config{AKAChallengeMode: "minimal"})
	request := eapaka.Packet{
		Type:       eapaka.TypeAKA,
		Attributes: []eapaka.Attribute{eapaka.CheckcodeAttribute(nil)},
	}

	attrs := session.appendAKAChallengeMetaAttrs(nil, request)
	checkcode, ok := eapaka.FindAttribute(attrs, eapaka.AttributeCheckcode)
	if !ok {
		t.Fatal("empty AT_CHECKCODE was omitted")
	}
	value, err := checkcode.CheckcodeValue()
	if err != nil {
		t.Fatalf("parse AT_CHECKCODE: %v", err)
	}
	if len(value) != 0 {
		t.Fatalf("AT_CHECKCODE = %x, want empty value", value)
	}
}

func TestCheckcodeModePreservesCompleteServerValue(t *testing.T) {
	want := bytes.Repeat([]byte{0x5a}, 20)
	session := NewSession(&Config{AKAChallengeMode: "checkcode"})
	request := eapaka.Packet{
		Type: eapaka.TypeAKA,
		Attributes: []eapaka.Attribute{
			eapaka.CheckcodeAttribute(want),
			eapaka.ResultIndAttribute(),
		},
	}

	attrs := session.appendAKAChallengeMetaAttrs(nil, request)
	checkcode, ok := eapaka.FindAttribute(attrs, eapaka.AttributeCheckcode)
	if !ok {
		t.Fatal("AT_CHECKCODE was omitted")
	}
	value, err := checkcode.CheckcodeValue()
	if err != nil {
		t.Fatalf("parse AT_CHECKCODE: %v", err)
	}
	if !bytes.Equal(value, want) {
		t.Fatalf("AT_CHECKCODE = %x, want %x", value, want)
	}
	if _, ok := eapaka.FindAttribute(attrs, eapaka.AttributeResultInd); !ok {
		t.Fatal("AT_RESULT_IND was omitted in checkcode mode")
	}
}

func TestLegacyIdentitySelectionAndKeyDerivationFallback(t *testing.T) {
	session := NewSession(&Config{IMSI: "234102356143376", FastReauthID: "fast@example"})
	if got := session.currentIKEIdentity(); got != "fast@example" {
		t.Fatalf("IKE identity = %q", got)
	}
	if got := session.currentEAPIdentity(); got != "fast@example" {
		t.Fatalf("EAP identity = %q", got)
	}
	session.eapIdentity, session.eapIdentitySet = "", true
	if got := session.currentEAPIdentity(); got != "" {
		t.Fatalf("explicit empty EAP identity = %q", got)
	}
	if got := session.currentEAPIdentityForKeyDerivation(); got != buildNAI(session.cfg.IMSI, session.cfg) {
		t.Fatalf("key derivation identity = %q", got)
	}
}

func TestBuildCPRequestPayloadHonorsIPStack(t *testing.T) {
	tests := []struct {
		mode string
		want []uint16
	}{
		{"ipv4", []uint16{1, 3, 20}},
		{"ipv6", []uint16{8, 10, 21, 16390}},
		{"", []uint16{1, 3, 20, 8, 10, 21, 16390}},
	}
	for _, test := range tests {
		session := NewSession(&Config{IPStackType: test.mode})
		payload := session.buildCPRequestPayload()
		if len(payload.Attributes) != len(test.want) {
			t.Fatalf("IPStack %q attributes = %d", test.mode, len(payload.Attributes))
		}
		for index, want := range test.want {
			attribute := payload.Attributes[index]
			if attribute.Type != want {
				t.Errorf("IPStack %q attribute[%d] = %d, want %d", test.mode, index, attribute.Type, want)
			}
			if want == ikev2.CPAttrIP6Address && (len(attribute.Value) != 17 || attribute.Value[16] != 64) {
				t.Errorf("IPv6 request = %x", attribute.Value)
			}
		}
	}
}

func notifyPayloads(payloads []ikev2.Payload) []*ikev2.EncryptedPayloadNotify {
	var notifications []*ikev2.EncryptedPayloadNotify
	for _, payload := range payloads {
		if notification, ok := payload.(*ikev2.EncryptedPayloadNotify); ok {
			notifications = append(notifications, notification)
		}
	}
	return notifications
}

// Spark NZ (530/05) answers an unsolicited DEVICE_IDENTITY with INVALID_SYNTAX.
func TestInitialIKEAuthNeverCarriesDeviceIdentity(t *testing.T) {
	for name, config := range map[string]*Config{
		"no IMEI":    {IMSI: "530050000000001", APN: "ims"},
		"IMEI":       {IMSI: "530050000000001", APN: "ims", DeviceIdentityIMEI: "490154203237518"},
		"spoof only": {IMSI: "530050000000001", APN: "ims", EnableDeviceIdentitySpoof: true},
	} {
		payloads, err := NewSession(config).buildIKEAuthInitPayloads()
		if err != nil {
			t.Fatalf("%s: buildIKEAuthInitPayloads: %v", name, err)
		}
		want := []uint16{
			ikev2.EAP_ONLY_AUTHENTICATION, ikev2.MOBIKE_SUPPORTED,
			ikev2.TICKET_REQUEST, ikev2.INITIAL_CONTACT,
		}
		notifications := notifyPayloads(payloads)
		if len(notifications) != len(want) {
			t.Fatalf("%s: notifications = %d, want %d", name, len(notifications), len(want))
		}
		for index, notifyType := range want {
			if got := notifications[index].NotifyType; got != notifyType {
				t.Errorf("%s: notification[%d] = %d, want %d", name, index, got, notifyType)
			}
		}
	}
}

func sentIKEAuthNotifies(t *testing.T, session *Session, transport *testIKETransport) []*ikev2.EncryptedPayloadNotify {
	t.Helper()
	packet, err := ikev2.DecodePacket(<-transport.sentIKE)
	if err != nil {
		t.Fatalf("DecodePacket: %v", err)
	}
	payloads, err := session.decryptAndParse(packet)
	if err != nil {
		t.Fatalf("decryptAndParse: %v", err)
	}
	return notifyPayloads(payloads)
}

func TestDeviceIdentityAnsweredOnceAfterEPDGRequest(t *testing.T) {
	session := NewSession(&Config{IMSI: "530050000000001", DeviceIdentityIMEI: "358983361433761"})
	transport := newTestIKETransport()
	session.socket = transport
	session.ikeKeys = testIKEKeys()
	session.stage = stageEAP
	session.responderAuthenticated = true // ePDG certificate AUTH already verified
	identityRequest := func(id byte) *ikev2.EncryptedPayloadEAP {
		return &ikev2.EncryptedPayloadEAP{EAPMessage: []byte{eapaka.CodeRequest, id, 0, 5, eapTypeIdentity}}
	}
	request := &ikev2.EncryptedPayloadNotify{
		ProtocolID: ikev2.ProtoIKE, NotifyType: ikev2.DEVICE_IDENTITY_3GPP, NotifyData: []byte{0, 1, 1},
	}

	if _, err := session.applyEAPHandlingResult([]ikev2.Payload{identityRequest(1)}); err != nil {
		t.Fatalf("unrequested: %v", err)
	}
	if got := sentIKEAuthNotifies(t, session, transport); len(got) != 0 {
		t.Fatalf("unrequested EAP response carried notifies %#v", got)
	}

	if _, err := session.applyEAPHandlingResult([]ikev2.Payload{identityRequest(2), request}); err != nil {
		t.Fatalf("requested: %v", err)
	}
	got := sentIKEAuthNotifies(t, session, transport)
	want := []byte{0x00, 0x09, 0x01, 0x53, 0x98, 0x38, 0x63, 0x41, 0x33, 0x67, 0xf1}
	if len(got) != 1 || got[0].NotifyType != ikev2.DEVICE_IDENTITY_3GPP || !bytes.Equal(got[0].NotifyData, want) {
		t.Fatalf("DEVICE_IDENTITY answer = %#v, want one 41101 with %x", got, want)
	}

	if _, err := session.applyEAPHandlingResult([]ikev2.Payload{identityRequest(3), request}); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if got := sentIKEAuthNotifies(t, session, transport); len(got) != 0 {
		t.Fatalf("DEVICE_IDENTITY answered twice: %#v", got)
	}
}

func TestDeviceIdentityWithheldUntilEPDGCertificateVerified(t *testing.T) {
	session := NewSession(&Config{IMSI: "530050000000001", DeviceIdentityIMEI: "358983361433761"})
	session.deviceIDRequested = true
	if payloads, err := session.pendingDeviceIdentity(); err != nil || payloads != nil {
		t.Fatalf("unauthenticated ePDG: payloads=%#v err=%v", payloads, err)
	}
	session.responderAuthenticated, session.eapOnlyAuthentication = true, true
	if payloads, err := session.pendingDeviceIdentity(); err != nil || payloads != nil {
		t.Fatalf("EAP-only ePDG without certificate: payloads=%#v err=%v", payloads, err)
	}
}

func TestOverlappingReauthOmitsInitialContact(t *testing.T) {
	session := NewSession(&Config{
		IMSI: "234102356143376", APN: "ims", OmitInitialContact: true,
	})
	payloads, err := session.buildIKEAuthInitPayloads()
	if err != nil {
		t.Fatalf("buildIKEAuthInitPayloads: %v", err)
	}
	for _, payload := range payloads {
		notification, ok := payload.(*ikev2.EncryptedPayloadNotify)
		if ok && notification.NotifyType == ikev2.INITIAL_CONTACT {
			t.Fatal("overlapping IKE_AUTH included INITIAL_CONTACT")
		}
	}
}

func TestSpoofAppleIMEIRestoresFixedTACAndLuhn(t *testing.T) {
	if got := spoofAppleIMEI("234102356143376"); got != "358983361433761" {
		t.Fatalf("spoofAppleIMEI = %q", got)
	}
	if got := spoofAppleIMEI("short"); got != "358983361234565" {
		t.Fatalf("short IMSI fallback = %q", got)
	}
}

func TestHandleEAPReturnsPayloadWithoutSending(t *testing.T) {
	transport := newTestIKETransport()
	session := NewSession(&Config{IMSI: "234102356143376"})
	session.socket = transport
	request := []byte{eapaka.CodeRequest, 7, 0, 5, eapTypeIdentity}
	payloads, err := session.handleEAP(request)
	if err != nil {
		t.Fatalf("handleEAP: %v", err)
	}
	if transport.sendCount.Load() != 0 {
		t.Fatal("handleEAP sent on the transport")
	}
	if len(payloads) != 1 || payloads[0].Type() != ikev2.PayloadEAP {
		t.Fatalf("response payloads = %#v", payloads)
	}
	response, err := eapaka.ParsePacket(payloads[0].(*ikev2.EncryptedPayloadEAP).EAPMessage)
	if err != nil {
		t.Fatalf("parse EAP response: %v", err)
	}
	if response.Code != eapaka.CodeResponse || response.Identifier != 7 || string(response.Data) != buildNAI(session.cfg.IMSI, session.cfg) {
		t.Fatalf("EAP identity response = %#v", response)
	}
}

func TestAKAIdentityWithoutIDRequestReturnsNoIdentityAttribute(t *testing.T) {
	session := NewSession(&Config{IMSI: "234102356143376"})
	request := eapaka.Packet{
		Code: eapaka.CodeRequest, Identifier: 9, Type: eapaka.TypeAKA,
		Subtype: eapaka.SubtypeIdentity,
	}
	raw, err := request.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	payloads, err := session.handleEAP(raw)
	if err != nil {
		t.Fatalf("handleEAP: %v", err)
	}
	response, err := eapaka.ParsePacket(payloads[0].(*ikev2.EncryptedPayloadEAP).EAPMessage)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(response.Attributes) != 0 || !session.eapIdentitySet || session.eapIdentity != "" {
		t.Fatalf("identity response = %#v, stored=%q/%t", response.Attributes, session.eapIdentity, session.eapIdentitySet)
	}
}

func TestDisableEAPMACValidationIsExplicitAndScoped(t *testing.T) {
	result := testAKAResult()
	strict := NewSession(&Config{
		IMSI: "234102356143376", AKAProvider: &recordingAKAProvider{result: result},
	})
	challenge := signedAKAChallenge(
		t, strict.currentEAPIdentity(), bytes.Repeat([]byte{0x31}, 16),
		bytes.Repeat([]byte{0x42}, 16), result,
	)
	challenge.Attributes[len(challenge.Attributes)-1].Data[2] ^= 0xff
	if _, err := strict.handleRFCChallenge(challenge); err == nil {
		t.Fatal("strict session accepted an invalid challenge MAC")
	}

	diagnostic := NewSession(&Config{
		IMSI: "234102356143376", SIM: &recordingAKAProvider{result: result},
		DisableEAPMACValidation: true,
	})
	if payloads, err := diagnostic.handleRFCChallenge(challenge); err != nil || len(payloads) != 1 {
		t.Fatalf("explicit diagnostic mode payloads=%d err=%v", len(payloads), err)
	}
}

func testAKAResult() AKAResult {
	return AKAResult{
		RES: bytes.Repeat([]byte{0x53}, 8),
		CK:  bytes.Repeat([]byte{0x14}, 16),
		IK:  bytes.Repeat([]byte{0x25}, 16),
	}
}

func TestMidEAPAuthenticationFailedStaysTyped(t *testing.T) {
	session := NewSession(&Config{IMSI: "530050000000001"})
	session.stage = stageEAP
	session.responderAuthenticated = true
	_, err := session.applyEAPHandlingResult([]ikev2.Payload{
		&ikev2.EncryptedPayloadNotify{NotifyType: ikev2.AUTHENTICATION_FAILED},
	})
	var authErr *IKEAuthError
	if !errors.As(err, &authErr) || authErr.NotifyType != ikev2.AUTHENTICATION_FAILED {
		t.Fatalf("mid-EAP rejection = %v, want IKEAuthError(24)", err)
	}
}

func TestDeviceIdentityRequestedWithEAPSuccessRidesFinalAUTH(t *testing.T) {
	session := NewSession(&Config{IMSI: "530050000000001", DeviceIdentityIMEI: "358983361433761"})
	session.stage = stageEAP
	session.responderAuthenticated = true
	session.eapKeys = eapaka.Keys{MSK: bytes.Repeat([]byte{0x11}, eapaka.KeyLengthMSK)}
	decision, err := session.applyEAPHandlingResult([]ikev2.Payload{
		&ikev2.EncryptedPayloadEAP{EAPMessage: []byte{eapaka.CodeSuccess, 4, 0, 4}},
		&ikev2.EncryptedPayloadNotify{NotifyType: ikev2.DEVICE_IDENTITY_3GPP, NotifyData: []byte{0, 1, 1}},
	})
	if err != nil || decision != "final" {
		t.Fatalf("decision=%q err=%v", decision, err)
	}
	payloads, err := session.pendingDeviceIdentity()
	if err != nil || len(payloads) != 1 {
		t.Fatalf("final AUTH device identity = %#v err=%v", payloads, err)
	}
}

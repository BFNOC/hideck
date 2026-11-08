package swu

import (
	"strings"
	"testing"

	"github.com/iniwex5/vowifi-go/engine/swu/eapaka"
	"go.uber.org/zap/zapcore"
)

func TestDevEAPFieldsLogShapeAndStatusButNotIdentity(t *testing.T) {
	const identity = "0515020123456789@nai.epc.mnc002.mcc515.3gppnetwork.org"
	raw, err := eapaka.Packet{
		Code: eapaka.CodeResponse, Identifier: 7, Type: 23, Subtype: eapaka.SubtypeIdentity,
		Attributes: []eapaka.Attribute{eapaka.IdentityAttribute(identity), eapaka.NotificationAttribute(16384)},
	}.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range devEAPFields(raw) {
		field.AddTo(encoder)
	}
	if encoder.Fields["code"] != uint8(eapaka.CodeResponse) || encoder.Fields["notification"] != int64(16384) {
		t.Fatalf("fields = %v, want code %d and notification 16384", encoder.Fields, eapaka.CodeResponse)
	}
	for key, value := range encoder.Fields {
		if text, ok := value.(string); ok && strings.Contains(text, "0515020123456789") {
			t.Fatalf("field %s leaks the identity: %q", key, text)
		}
	}
}

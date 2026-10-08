package swu

import (
	"strings"
	"testing"

	"github.com/iniwex5/vowifi-go/engine/ikev2"
)

func TestCreateChildSAOfferDescribesEveryProposal(t *testing.T) {
	first := &ikev2.Proposal{ProposalNum: 1, ProtocolID: ikev2.ProtoESP, SPI: []byte{1, 2, 3, 4}}
	first.AddTransform(ikev2.TransformTypeEncr, 12, 256)
	first.AddTransform(ikev2.TransformTypeInteg, 12, 0)
	second := &ikev2.Proposal{ProposalNum: 2, ProtocolID: ikev2.ProtoESP, SPI: []byte{1, 2, 3, 4}}
	second.AddTransform(ikev2.TransformTypeEncr, 20, 128)
	payloads := []ikev2.Payload{
		&ikev2.EncryptedPayloadSA{Proposals: []*ikev2.Proposal{first, second}},
		&ikev2.EncryptedPayloadNotify{NotifyType: ikev2.REKEY_SA},
	}
	got := createChildSAOffer(payloads)
	for _, want := range []string{"SA#1(proto=3 spi=4 1=12/256,3=12)", "SA#2(proto=3 spi=4 1=20/128)", "N(16393)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("offer %q missing %q", got, want)
		}
	}
	if protocol, err := createChildSAProtocol(payloads); err != nil || protocol != ikev2.ProtoESP {
		t.Fatalf("multi-proposal protocol = %d, %v", protocol, err)
	}
	if _, err := createChildSAProtocol([]ikev2.Payload{&ikev2.EncryptedPayloadNotify{NotifyType: ikev2.REKEY_SA}}); err == nil ||
		!strings.Contains(err.Error(), "payloads: N(16393)") {
		t.Fatalf("createChildSAProtocol error = %v", err)
	}
}

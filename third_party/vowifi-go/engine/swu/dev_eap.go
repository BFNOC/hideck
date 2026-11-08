package swu

// [dev] debug-field: the shape of every EAP packet in both directions, so a
// run shows at which step an ePDG answers EAP-Failure (DITO sometimes rejects
// the first IKE_AUTH outright, before any EAP request). Only code, identifier,
// type, subtype and attribute numbers are logged, plus the AT_NOTIFICATION
// and AT_CLIENT_ERROR_CODE status codes. Identities, RAND, AUTN, RES, AUTS,
// MACs and encrypted attributes are never logged. Debug-field branch only.

import (
	"encoding/binary"

	"github.com/iniwex5/vowifi-go/engine/ikev2"
	"github.com/iniwex5/vowifi-go/engine/swu/eapaka"
	"go.uber.org/zap"
)

func (s *Session) devLogEAP(direction string, raw []byte) {
	s.devLogger().Info("[dev] EAP "+direction, devEAPFields(raw)...)
}

func (s *Session) devLogEAPPayloads(direction string, payloads []ikev2.Payload) {
	for _, payload := range payloads {
		if data, ok := ikeEAPData(payload); ok {
			s.devLogEAP(direction, data)
		}
	}
}

func devEAPFields(raw []byte) []zap.Field {
	packet, err := eapaka.ParsePacket(raw)
	if err != nil {
		return []zap.Field{zap.Int("bytes", len(raw)), zap.Error(err)}
	}
	fields := []zap.Field{
		zap.Uint8("code", packet.Code), zap.Uint8("identifier", packet.Identifier),
		zap.Uint8("type", packet.Type), zap.Uint8("subtype", packet.Subtype), zap.Int("bytes", len(raw)),
	}
	attributes := make([]int, 0, len(packet.Attributes))
	for _, attribute := range packet.Attributes {
		attributes = append(attributes, int(attribute.Type))
		switch attribute.Type {
		case eapaka.AttributeNotification:
			fields = append(fields, zap.Int("notification", devEAPStatusCode(attribute.Data)))
		case eapaka.AttributeClientErrorCode:
			fields = append(fields, zap.Int("client_error_code", devEAPStatusCode(attribute.Data)))
		}
	}
	return append(fields, zap.Ints("attributes", attributes))
}

// devEAPStatusCode reads the 2-byte value of a SIM/AKA status attribute, or -1
// when the attribute is too short.
func devEAPStatusCode(data []byte) int {
	if len(data) < 2 {
		return -1
	}
	return int(binary.BigEndian.Uint16(data[:2]))
}

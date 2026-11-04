package ipsec

// [dev] one line per IKE packet in either direction, header fields only:
// exchange, initiator/response flags, message ID, SPIs, first payload and
// length. Payloads stay encrypted, so nothing secret reaches the log.
// Debug-field branch only.

import (
	"encoding/binary"
	"encoding/hex"

	"go.uber.org/zap"
)

var devIKEExchanges = map[byte]string{
	34: "IKE_SA_INIT", 35: "IKE_AUTH", 36: "CREATE_CHILD_SA", 37: "INFORMATIONAL", 38: "IKE_SESSION_RESUME",
}

func devLogIKE(direction, local, remote string, packet []byte) {
	if len(packet) < 28 {
		logInfo("[dev] IKE packet too short", zap.String("dir", direction), zap.Int("bytes", len(packet)))
		return
	}
	exchange, ok := devIKEExchanges[packet[18]]
	if !ok {
		exchange = "type-" + hex.EncodeToString(packet[18:19])
	}
	flags := packet[19]
	logInfo("[dev] IKE "+direction,
		zap.String("exchange", exchange),
		zap.Bool("response", flags&0x20 != 0), zap.Bool("initiator", flags&0x08 != 0),
		zap.Uint32("msg_id", binary.BigEndian.Uint32(packet[20:24])),
		zap.String("spi_i", hex.EncodeToString(packet[0:8])), zap.String("spi_r", hex.EncodeToString(packet[8:16])),
		zap.Uint8("next_payload", packet[16]), zap.Uint32("length", binary.BigEndian.Uint32(packet[24:28])),
		zap.Int("bytes", len(packet)), zap.String("local", local), zap.String("remote", remote))
}

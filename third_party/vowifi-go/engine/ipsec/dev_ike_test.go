package ipsec

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	enginelogger "github.com/iniwex5/vowifi-go/engine/logger"
)

func TestDevLogIKEReadsTheHeader(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	enginelogger.Set(zap.New(core))
	t.Cleanup(func() { enginelogger.Set(zap.NewNop()) })
	packet := make([]byte, 40)
	packet[7], packet[15] = 0x11, 0x22
	packet[16], packet[17], packet[18], packet[19] = 46, 0x20, 37, 0x20|0x08
	packet[23], packet[27] = 5, 40
	devLogIKE("rx", "l", "r", packet)
	devLogIKE("tx", "l", "r", packet[:10])
	entries := logs.All()
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	fields := entries[0].ContextMap()
	if entries[0].Message != "[dev] IKE rx" || fields["exchange"] != "INFORMATIONAL" || fields["response"] != true ||
		fields["initiator"] != true || fields["msg_id"] != uint32(5) || fields["spi_i"] != "0000000000000011" {
		t.Fatalf("fields = %v", fields)
	}
	if entries[1].Message != "[dev] IKE packet too short" {
		t.Fatalf("short packet logged as %q", entries[1].Message)
	}
}

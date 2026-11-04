package swu

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/iniwex5/vowifi-go/engine/ikev2"
)

func TestDevESPStatsSummaryReportsDeltasAndAge(t *testing.T) {
	var stats devESPStats
	stats.record(false, 0x11, 100, false)
	stats.record(false, 0x11, 50, false)
	stats.record(false, 0x22, 0, true)
	first := stats.summary(false)
	if !strings.Contains(first, "00000011:pkts=2(+2),bytes=150,err=0,last=") ||
		!strings.Contains(first, "00000022:pkts=0(+0),bytes=0,err=1,last=never") {
		t.Fatalf("summary = %s", first)
	}
	if second := stats.summary(false); !strings.Contains(second, "00000011:pkts=2(+0)") {
		t.Fatalf("second summary = %s", second)
	}
	if out := stats.summary(true); out != "" {
		t.Fatalf("outbound summary = %q", out)
	}
}

func TestDevLogIKEPayloadsNamesNotifyDeleteAndSPIs(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	s := &Session{Logger: zap.New(core)}
	header := &ikev2.IKEHeader{ExchangeType: ikev2.INFORMATIONAL, Flags: 0x20}
	s.devLogIKEPayloads("rx", header, 7, []ikev2.Payload{
		&ikev2.EncryptedPayloadDelete{ProtocolID: ikev2.ProtoESP, SPISize: 4, NumSPIs: 1, SPIs: []byte{0xc1, 0x00, 0x00, 0x02}},
		&ikev2.EncryptedPayloadNotify{NotifyType: 16393, ProtocolID: ikev2.ProtoESP, SPI: []byte{1, 2, 3, 4}},
		&ikev2.EncryptedPayloadID{IDData: []byte("262021234567890@nai.epc")},
	})
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	fields := entries[0].ContextMap()
	if fields["payloads"] != "D,N,IDr" || fields["delete"] != "proto=3:c1000002" ||
		!strings.Contains(fields["notify"].(string), "16393(proto=3,spi=01020304") || fields["response"] != true {
		t.Fatalf("fields = %v", fields)
	}
	for _, value := range fields {
		if text, ok := value.(string); ok && strings.Contains(text, "262021") {
			t.Fatalf("identity reached the log: %v", fields)
		}
	}
}

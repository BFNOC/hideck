package swu

// [dev] debug-field: CHILD_SA lifecycle, decrypted IKE payload summaries and
// per-SPI ESP counters, so a data-plane stall can be placed on one SA and one
// direction from a single run. Payload contents (identities, EAP, keys,
// nonces, configuration) are never logged; only payload types, notify types,
// SPIs and traffic selectors. Debug-field branch only.

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/iniwex5/vowifi-go/engine/ikev2"
	"github.com/iniwex5/vowifi-go/engine/logger"
)

type devESPCounter struct {
	packets, bytes, errors atomic.Uint64
	last                   atomic.Int64
	// reported is the packet count at the previous summary.
	reported atomic.Uint64
}

type devESPStats struct {
	mu  sync.Mutex
	in  map[uint32]*devESPCounter
	out map[uint32]*devESPCounter
}

func (d *devESPStats) counter(outbound bool, spi uint32) *devESPCounter {
	d.mu.Lock()
	defer d.mu.Unlock()
	table := &d.in
	if outbound {
		table = &d.out
	}
	if *table == nil {
		*table = make(map[uint32]*devESPCounter)
	}
	counter := (*table)[spi]
	if counter == nil {
		counter = &devESPCounter{}
		(*table)[spi] = counter
	}
	return counter
}

func (d *devESPStats) record(outbound bool, spi uint32, size int, failed bool) {
	counter := d.counter(outbound, spi)
	if failed {
		counter.errors.Add(1)
		return
	}
	counter.packets.Add(1)
	counter.bytes.Add(uint64(size))
	counter.last.Store(time.Now().UnixNano())
}

// summary lists every SPI seen with its totals, the packets since the last
// summary and how long ago the last packet went through.
func (d *devESPStats) summary(outbound bool) string {
	d.mu.Lock()
	table := d.in
	if outbound {
		table = d.out
	}
	spis := make([]uint32, 0, len(table))
	for spi := range table {
		spis = append(spis, spi)
	}
	d.mu.Unlock()
	sort.Slice(spis, func(i, j int) bool { return spis[i] < spis[j] })
	now := time.Now()
	parts := make([]string, 0, len(spis))
	for _, spi := range spis {
		counter := d.counter(outbound, spi)
		packets := counter.packets.Load()
		delta := packets - counter.reported.Swap(packets)
		age := "never"
		if last := counter.last.Load(); last != 0 {
			age = now.Sub(time.Unix(0, last)).Round(time.Millisecond).String()
		}
		parts = append(parts, fmt.Sprintf("%08x:pkts=%d(+%d),bytes=%d,err=%d,last=%s",
			spi, packets, delta, counter.bytes.Load(), counter.errors.Load(), age))
	}
	return strings.Join(parts, " ")
}

func (s *Session) devLogger() *zap.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return logger.L()
}

func (s *Session) devLogESPSummary(kind string, totals dataPlaneStatsSnapshot) {
	snapshot := s.captureChildSASnapshot()
	s.devLogger().Info("[dev] SWu ESP per SA", zap.String("kind", kind), zap.String("session", fmt.Sprintf("%p", s)),
		zap.Uint64("esp_in", totals.espIn), zap.Uint64("esp_send", totals.espSend),
		zap.Uint64("esp_send_error", totals.espSendError), zap.Uint64("esp_out_sa_miss", totals.espOutSAMiss),
		zap.Uint64("esp_encap_error", totals.espEncapError), zap.Uint64("esp_in_sa_miss", totals.espInSAMiss),
		zap.Uint64("esp_decap_error", totals.espDecapError), zap.Uint64("tun_write_error", totals.tunWriteError),
		zap.String("current_local_spi", fmt.Sprintf("%08x", snapshot.localSPI)),
		zap.String("current_remote_spi", fmt.Sprintf("%08x", snapshot.remoteSPI)),
		zap.String("inbound_spis", snapshot.inboundSPIText()),
		zap.String("in", s.devESP.summary(false)), zap.String("out", s.devESP.summary(true)),
		devOuterFields(s.transport()))
}

type devOuterCounters interface {
	DevOuterCounters() (keepalives, sendFailures, unknownSources uint64)
}

func devOuterFields(transport any) zap.Field {
	counters, ok := transport.(devOuterCounters)
	if !ok {
		return zap.Skip()
	}
	keepalives, failures, unknown := counters.DevOuterCounters()
	return zap.String("outer", fmt.Sprintf("nat_keepalives=%d,send_failures=%d,unknown_sources=%d", keepalives, failures, unknown))
}

func (s *Session) devLogChildSA(event string, fields ...zap.Field) {
	snapshot := s.captureChildSASnapshot()
	s.devLogger().Info("[dev] CHILD_SA "+event, append(fields, zap.String("session", fmt.Sprintf("%p", s)),
		zap.String("current_local_spi", fmt.Sprintf("%08x", snapshot.localSPI)),
		zap.String("current_remote_spi", fmt.Sprintf("%08x", snapshot.remoteSPI)),
		zap.String("inbound_spis", snapshot.inboundSPIText()))...)
}

var devIKEPayloadNames = map[ikev2.PayloadType]string{
	33: "SA", 34: "KE", 35: "IDi", 36: "IDr", 37: "CERT", 38: "CERTREQ", 39: "AUTH", 40: "Nonce",
	41: "N", 42: "D", 43: "V", 44: "TSi", 45: "TSr", 46: "SK", 47: "CP", 48: "EAP", 53: "SKF",
}

// devLogIKEPayloads summarizes one protected IKE message after decryption or
// before encryption.
func (s *Session) devLogIKEPayloads(direction string, header *ikev2.IKEHeader, msgID uint32, payloads []ikev2.Payload) {
	names := make([]string, 0, len(payloads))
	var notifies, deletes, proposals, selectors []string
	for _, payload := range payloads {
		if payload == nil {
			names = append(names, "nil")
			continue
		}
		name := devIKEPayloadNames[payload.Type()]
		if name == "" {
			name = fmt.Sprintf("type-%d", payload.Type())
		}
		names = append(names, name)
		switch p := payload.(type) {
		case *ikev2.EncryptedPayloadNotify:
			notifies = append(notifies, fmt.Sprintf("%d(proto=%d,spi=%s,data_len=%d)",
				p.NotifyType, p.ProtocolID, hex.EncodeToString(p.SPI), len(p.NotifyData)))
		case *ikev2.EncryptedPayloadDelete:
			var spis []string
			for raw := p.SPIs; p.SPISize == 4 && len(raw) >= 4; raw = raw[4:] {
				spis = append(spis, fmt.Sprintf("%08x", binary.BigEndian.Uint32(raw[:4])))
			}
			if p.SPISize != 4 {
				spis = append(spis, fmt.Sprintf("spi_size=%d", p.SPISize))
			}
			deletes = append(deletes, fmt.Sprintf("proto=%d:%s", p.ProtocolID, strings.Join(spis, ",")))
		case *ikev2.EncryptedPayloadSA:
			for _, proposal := range p.Proposals {
				if proposal != nil {
					proposals = append(proposals, fmt.Sprintf("proto=%d,spi=%s", proposal.ProtocolID, hex.EncodeToString(proposal.SPI)))
				}
			}
		case *ikev2.EncryptedPayloadTS:
			list := p.TrafficSelectors
			if len(list) == 0 {
				list = p.Selectors
			}
			for _, selector := range list {
				if selector != nil {
					selectors = append(selectors, fmt.Sprintf("%s:%s-%s/%d:%d-%d", name,
						net.IP(selector.StartAddr), net.IP(selector.EndAddr), selector.IPProtocol,
						selector.StartPort, selector.EndPort))
				}
			}
		}
	}
	fields := []zap.Field{zap.Uint32("msg_id", msgID), zap.String("payloads", strings.Join(names, ","))}
	if header != nil {
		fields = append(fields, zap.String("session", fmt.Sprintf("%p", s)),
			zap.String("ike_spi_i", fmt.Sprintf("%016x", header.SPIi)),
			zap.String("ike_spi_r", fmt.Sprintf("%016x", header.SPIr)),
			zap.Uint8("exchange", uint8(header.ExchangeType)),
			zap.Bool("response", header.Flags&0x20 != 0), zap.Bool("initiator", header.Flags&0x08 != 0))
	}
	if len(notifies) > 0 {
		fields = append(fields, zap.String("notify", strings.Join(notifies, " ")))
	}
	if len(deletes) > 0 {
		fields = append(fields, zap.String("delete", strings.Join(deletes, " ")))
	}
	if len(proposals) > 0 {
		fields = append(fields, zap.String("sa_proposals", strings.Join(proposals, " ")))
	}
	if len(selectors) > 0 {
		fields = append(fields, zap.String("traffic_selectors", strings.Join(selectors, " ")))
	}
	s.devLogger().Info("[dev] IKE payloads "+direction, fields...)
}

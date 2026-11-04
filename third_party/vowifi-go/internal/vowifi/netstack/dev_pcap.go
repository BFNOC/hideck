package netstack

// [dev] header-only pcapng of the inner IMS packets as the packet bridge sees
// them, plaintext side of the 3GPP IPsec transform. On by default in the
// debug-field branch, written under logs/dev-pcap next to app.log;
// HIDECK_DEV_PCAP_DIR moves it and "off" turns it off. Never part of a PR.
//
// Each packet keeps its IP and TCP/UDP headers and loses the payload, so no
// SIP text (IMPU, IMEI, numbers) reaches the file. The packet comment says
// whether it crossed an IMS SA ("esp spi=…"), went plaintext, or was dropped
// and why, plus the payload length and, for SIP, only the method or status
// code and the CSeq.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

const (
	devPcapDirEnv     = "HIDECK_DEV_PCAP_DIR"
	devPcapDefaultDir = "logs/dev-pcap"
	devPcapMaxBytes   = 64 << 20
	// Queued blocks before record starts dropping instead of blocking.
	devPcapQueueDepth = 4096
	devPcapMaxComment = 512
	// LINKTYPE_RAW: the block starts with the IPv4 or IPv6 header.
	devPcapLinkTypeRaw = 101
)

var (
	devPcapSeq        atomic.Uint64
	devSIPMethodToken = regexp.MustCompile(`^[A-Z]{3,10}$`)
	devSIPCSeqValue   = regexp.MustCompile(`^[0-9]{1,10} [A-Z]{3,10}$`)
)

type devPcap struct {
	path  string
	file  *os.File
	queue chan []byte
	done  chan struct{}
	// mu orders record's queue send against Close.
	mu      sync.RWMutex
	closed  bool
	full    atomic.Bool
	dropped atomic.Uint64
	// written and packets belong to the writer goroutine until done closes.
	written int64
	packets uint64
}

func openDevPcap() *devPcap {
	dir := strings.TrimSpace(os.Getenv(devPcapDirEnv))
	switch strings.ToLower(dir) {
	case "":
		if testing.Testing() {
			return nil
		}
		dir = devPcapDefaultDir
	case "off":
		return nil
	}
	name := fmt.Sprintf("ims-inner-%s-%d.pcapng", time.Now().Format("20060102-150405"), devPcapSeq.Add(1))
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logging.Info("[dev] IMS inner pcap unavailable", "path", path, "err", err)
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		logging.Info("[dev] IMS inner pcap unavailable", "path", path, "err", err)
		return nil
	}
	header := append(pcapngSectionHeader(), pcapngInterfaceDescription()...)
	if _, err := file.Write(header); err != nil {
		_ = file.Close()
		logging.Info("[dev] IMS inner pcap unavailable", "path", path, "err", err)
		return nil
	}
	p := &devPcap{
		path: path, file: file, written: int64(len(header)),
		queue: make(chan []byte, devPcapQueueDepth), done: make(chan struct{}),
	}
	go p.writeLoop()
	logging.Info("[dev] IMS inner pcap opened", "path", path)
	return p
}

// record queues one packet; a full queue drops it rather than delay the
// packet path, and Close reports how many were dropped.
func (p *devPcap) record(at time.Time, packet []byte, note string) {
	if p == nil || len(packet) == 0 || p.full.Load() {
		return
	}
	keep, summary := devPcapCut(packet)
	if summary != "" {
		note += " " + summary
	}
	if len(note) > devPcapMaxComment {
		note = note[:devPcapMaxComment]
	}
	block := pcapngEnhancedPacket(at, packet[:keep], len(packet), note)
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return
	}
	select {
	case p.queue <- block:
	default:
		p.dropped.Add(1)
	}
}

func (p *devPcap) writeLoop() {
	defer close(p.done)
	for block := range p.queue {
		if p.file == nil {
			continue
		}
		if p.written+int64(len(block)) > devPcapMaxBytes {
			if !p.full.Swap(true) {
				logging.Info("[dev] IMS inner pcap reached its size cap; capture stopped",
					"path", p.path, "bytes", p.written, "packets", p.packets)
			}
			continue
		}
		if _, err := p.file.Write(block); err != nil {
			logging.Info("[dev] IMS inner pcap write failed; capture stopped", "path", p.path, "err", err)
			_ = p.file.Close()
			p.file = nil
			p.full.Store(true)
			continue
		}
		p.written += int64(len(block))
		p.packets++
	}
}

func (p *devPcap) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.queue)
	p.mu.Unlock()
	<-p.done
	var err error
	if p.file != nil {
		err = p.file.Close()
		p.file = nil
	}
	logging.Info("[dev] IMS inner pcap closed", "path", p.path, "bytes", p.written,
		"packets", p.packets, "dropped", p.dropped.Load(), "err", err)
}

// devPcapCut returns how many leading bytes to keep and a summary of what was
// cut. It keeps only headers it has parsed; anything it cannot place (a
// non-first fragment, an unknown protocol, a malformed packet) keeps no bytes
// past the last header it is sure of.
func devPcapCut(packet []byte) (int, string) {
	ipLen, protocol, laterFragment, ok := devIPHeader(packet, false)
	if !ok {
		return 0, "unparsed len=" + strconv.Itoa(len(packet))
	}
	rest := packet[ipLen:]
	if laterFragment {
		return ipLen, "fragment len=" + strconv.Itoa(len(rest))
	}
	switch protocol {
	case 6: // TCP
		if len(rest) < 20 {
			return ipLen, ""
		}
		header := int(rest[12]>>4) * 4
		if header < 20 || header > len(rest) {
			return ipLen + 20, ""
		}
		return ipLen + header, devPayloadSummary(rest[header:])
	case 17: // UDP
		if len(rest) < 8 {
			return ipLen, ""
		}
		return ipLen + 8, devPayloadSummary(rest[8:])
	case 50: // ESP: SPI and sequence number only
		return ipLen + min(len(rest), 8), ""
	case 1, 58:
		return ipLen + devICMPKeep(protocol, rest), ""
	default:
		return ipLen, ""
	}
}

// devICMPKeep keeps the ICMP header and, for an error, the headers of the
// packet it quotes but none of that packet's payload.
func devICMPKeep(protocol byte, icmp []byte) int {
	if len(icmp) < 8 {
		return 0
	}
	kind := icmp[0]
	isError := kind < 128
	if protocol == 1 {
		isError = kind == 3 || kind == 4 || kind == 5 || kind == 11 || kind == 12
	}
	if !isError {
		return 8
	}
	quoted := icmp[8:]
	quotedLen, quotedProtocol, laterFragment, ok := devIPHeader(quoted, true)
	if !ok {
		return 8
	}
	if laterFragment {
		return 8 + quotedLen
	}
	available := len(quoted) - quotedLen
	switch quotedProtocol {
	case 6:
		return 8 + quotedLen + min(available, 20)
	case 17, 50:
		return 8 + quotedLen + min(available, 8)
	default:
		return 8 + quotedLen
	}
}

// devIPHeader returns the IP header length including IPv6 extension headers,
// the upper-layer protocol, and whether this is a non-first fragment. A
// quoted packet (inside an ICMP error) may be cut short; any other packet
// must match its own length field.
func devIPHeader(packet []byte, quoted bool) (int, byte, bool, bool) {
	if len(packet) == 0 {
		return 0, 0, false, false
	}
	switch packet[0] >> 4 {
	case 4:
		if len(packet) < 20 {
			return 0, 0, false, false
		}
		ipLen := int(packet[0]&0x0f) * 4
		total := int(binary.BigEndian.Uint16(packet[2:4]))
		if ipLen < 20 || ipLen > len(packet) || total < ipLen || (!quoted && total != len(packet)) {
			return 0, 0, false, false
		}
		laterFragment := binary.BigEndian.Uint16(packet[6:8])&0x1fff != 0
		return ipLen, packet[9], laterFragment, true
	case 6:
		if len(packet) < 40 || (!quoted && 40+int(binary.BigEndian.Uint16(packet[4:6])) != len(packet)) {
			return 0, 0, false, false
		}
		offset, next := 40, packet[6]
		for {
			switch next {
			case 0, 43, 60: // hop-by-hop, routing, destination options
				if len(packet) < offset+2 {
					return 0, 0, false, false
				}
				length := (int(packet[offset+1]) + 1) * 8
				if len(packet) < offset+length {
					return 0, 0, false, false
				}
				next, offset = packet[offset], offset+length
			case 44: // fragment
				if len(packet) < offset+8 {
					return 0, 0, false, false
				}
				laterFragment := binary.BigEndian.Uint16(packet[offset+2:offset+4])>>3 != 0
				next, offset = packet[offset], offset+8
				if laterFragment {
					return offset, next, true, true
				}
			case 51: // AH
				if len(packet) < offset+2 {
					return 0, 0, false, false
				}
				length := (int(packet[offset+1]) + 2) * 4
				if len(packet) < offset+length {
					return 0, 0, false, false
				}
				next, offset = packet[offset], offset+length
			default:
				return offset, next, false, true
			}
		}
	default:
		return 0, 0, false, false
	}
}

// devESPNote names the SA of an ESP packet, or returns "" for anything else.
func devESPNote(packet []byte) string {
	ipLen, protocol, laterFragment, ok := devIPHeader(packet, false)
	if !ok || laterFragment || protocol != 50 || len(packet) < ipLen+8 {
		return ""
	}
	esp := packet[ipLen:]
	return fmt.Sprintf("esp spi=%d seq=%d", binary.BigEndian.Uint32(esp[:4]), binary.BigEndian.Uint32(esp[4:8]))
}

func devPayloadSummary(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	summary := "len=" + strconv.Itoa(len(payload))
	switch string(payload) {
	case "\r\n\r\n":
		return summary + " crlf-ping"
	case "\r\n":
		return summary + " crlf-pong"
	}
	line := payload
	if end := bytes.Index(line, []byte("\r\n")); end >= 0 {
		line = line[:end]
	}
	fields := strings.Fields(string(line))
	switch {
	case len(fields) >= 2 && fields[0] == "SIP/2.0" && len(fields[1]) == 3:
		if _, err := strconv.Atoi(fields[1]); err != nil {
			return summary
		}
		summary += " sip=" + fields[1]
	case len(fields) == 3 && fields[2] == "SIP/2.0" && devSIPMethodToken.MatchString(fields[0]):
		summary += " sip=" + fields[0]
	default:
		return summary
	}
	if cseq := devSIPCSeq(payload); cseq != "" {
		summary += " cseq=" + strings.ReplaceAll(cseq, " ", "_")
	}
	return summary
}

func devSIPCSeq(message []byte) string {
	for _, line := range strings.Split(string(message), "\r\n") {
		if line == "" {
			return ""
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "cseq") {
			continue
		}
		value = strings.Join(strings.Fields(value), " ")
		if devSIPCSeqValue.MatchString(value) {
			return value
		}
		return ""
	}
	return ""
}

func pcapngSectionHeader() []byte {
	block := make([]byte, 28)
	binary.LittleEndian.PutUint32(block[0:], 0x0A0D0D0A)
	binary.LittleEndian.PutUint32(block[4:], 28)
	binary.LittleEndian.PutUint32(block[8:], 0x1A2B3C4D)
	binary.LittleEndian.PutUint16(block[12:], 1)
	binary.LittleEndian.PutUint16(block[14:], 0)
	binary.LittleEndian.PutUint64(block[16:], ^uint64(0))
	binary.LittleEndian.PutUint32(block[24:], 28)
	return block
}

func pcapngInterfaceDescription() []byte {
	block := make([]byte, 20)
	binary.LittleEndian.PutUint32(block[0:], 1)
	binary.LittleEndian.PutUint32(block[4:], 20)
	binary.LittleEndian.PutUint16(block[8:], devPcapLinkTypeRaw)
	binary.LittleEndian.PutUint32(block[12:], 0)
	binary.LittleEndian.PutUint32(block[16:], 20)
	return block
}

func pcapngEnhancedPacket(at time.Time, data []byte, originalLength int, comment string) []byte {
	pad := func(n int) int { return (n + 3) &^ 3 }
	options := 0
	if comment != "" {
		options = 4 + pad(len(comment)) + 4
	}
	total := 28 + pad(len(data)) + options + 4
	block := make([]byte, total)
	micros := uint64(at.UnixMicro())
	binary.LittleEndian.PutUint32(block[0:], 6)
	binary.LittleEndian.PutUint32(block[4:], uint32(total))
	binary.LittleEndian.PutUint32(block[8:], 0)
	binary.LittleEndian.PutUint32(block[12:], uint32(micros>>32))
	binary.LittleEndian.PutUint32(block[16:], uint32(micros))
	binary.LittleEndian.PutUint32(block[20:], uint32(len(data)))
	binary.LittleEndian.PutUint32(block[24:], uint32(originalLength))
	copy(block[28:], data)
	offset := 28 + pad(len(data))
	if comment != "" {
		binary.LittleEndian.PutUint16(block[offset:], 1)
		binary.LittleEndian.PutUint16(block[offset+2:], uint16(len(comment)))
		copy(block[offset+4:], comment)
		// opt_endofopt stays zero.
	}
	binary.LittleEndian.PutUint32(block[total-4:], uint32(total))
	return block
}

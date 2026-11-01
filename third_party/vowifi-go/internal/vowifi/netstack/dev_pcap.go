package netstack

// [dev] hideck#90: header-only pcapng of the inner IMS packets as the packet
// bridge sees them, plaintext side of the 3GPP IPsec transform. Enabled only
// when HIDECK_DEV90_PCAP_DIR is set; remove before a PR.
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
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

const (
	devPcapDirEnv   = "HIDECK_DEV90_PCAP_DIR"
	devPcapMaxBytes = 64 << 20
	// LINKTYPE_RAW: the block starts with the IPv4 or IPv6 header.
	devPcapLinkTypeRaw = 101
)

var (
	devPcapSeq        atomic.Uint64
	devSIPMethodToken = regexp.MustCompile(`^[A-Z]{3,10}$`)
	devSIPCSeqValue   = regexp.MustCompile(`^[0-9]{1,10} [A-Z]{3,10}$`)
)

type devPcap struct {
	mu      sync.Mutex
	file    *os.File
	path    string
	written int64
	packets uint64
	full    bool
}

func openDevPcap() *devPcap {
	dir := strings.TrimSpace(os.Getenv(devPcapDirEnv))
	if dir == "" {
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
	logging.Info("[dev] IMS inner pcap opened", "path", path)
	return &devPcap{file: file, path: path, written: int64(len(header))}
}

func (p *devPcap) record(packet []byte, note string) {
	if p == nil || len(packet) == 0 {
		return
	}
	keep, summary := devPcapCut(packet)
	if summary != "" {
		note += " " + summary
	}
	block := pcapngEnhancedPacket(time.Now(), packet[:keep], len(packet), note)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.file == nil || p.full {
		return
	}
	if p.written+int64(len(block)) > devPcapMaxBytes {
		p.full = true
		logging.Info("[dev] IMS inner pcap reached its size cap; capture stopped",
			"path", p.path, "bytes", p.written, "packets", p.packets)
		return
	}
	if _, err := p.file.Write(block); err != nil {
		logging.Info("[dev] IMS inner pcap write failed; capture stopped", "path", p.path, "err", err)
		_ = p.file.Close()
		p.file = nil
		return
	}
	p.written += int64(len(block))
	p.packets++
}

func (p *devPcap) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.file == nil {
		return
	}
	err := p.file.Close()
	p.file = nil
	logging.Info("[dev] IMS inner pcap closed", "path", p.path, "bytes", p.written, "packets", p.packets, "err", err)
}

// devPcapCut returns how many leading bytes to keep (headers only) and a
// summary of the dropped payload.
func devPcapCut(packet []byte) (int, string) {
	ipLen, protocol, ok := devIPHeader(packet)
	if !ok {
		return min(len(packet), 40), ""
	}
	rest := packet[ipLen:]
	switch protocol {
	case 6: // TCP
		if len(rest) < 20 {
			return len(packet), ""
		}
		header := int(rest[12]>>4) * 4
		if header < 20 || header > len(rest) {
			return ipLen + 20, ""
		}
		return ipLen + header, devPayloadSummary(rest[header:])
	case 17: // UDP
		if len(rest) < 8 {
			return len(packet), ""
		}
		return ipLen + 8, devPayloadSummary(rest[8:])
	case 50: // ESP: SPI and sequence number only
		return ipLen + min(len(rest), 8), ""
	case 1: // ICMP errors quote the IP header and 8 bytes of the original
		return ipLen + min(len(rest), 8+20+8), ""
	case 58: // ICMPv6 quotes the original IPv6 and TCP headers; cut before any payload
		return ipLen + min(len(rest), 8+40+20), ""
	default:
		return ipLen, ""
	}
}

func devIPHeader(packet []byte) (int, byte, bool) {
	if len(packet) == 0 {
		return 0, 0, false
	}
	switch packet[0] >> 4 {
	case 4:
		if len(packet) < 20 {
			return 0, 0, false
		}
		ipLen := int(packet[0]&0x0f) * 4
		if ipLen < 20 || ipLen > len(packet) {
			return 0, 0, false
		}
		return ipLen, packet[9], true
	case 6:
		if len(packet) < 40 {
			return 0, 0, false
		}
		return 40, packet[6], true
	default:
		return 0, 0, false
	}
}

// devESPNote names the SA of an ESP packet, or returns "" for anything else.
func devESPNote(packet []byte) string {
	ipLen, protocol, ok := devIPHeader(packet)
	if !ok || protocol != 50 || len(packet) < ipLen+8 {
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

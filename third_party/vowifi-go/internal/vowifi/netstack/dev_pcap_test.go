package netstack

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type devPcapPacket struct {
	data     []byte
	original int
	comment  string
}

func readDevPcap(t *testing.T, path string) []devPcapPacket {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var packets []devPcapPacket
	for offset := 0; offset < len(raw); {
		if len(raw)-offset < 12 {
			t.Fatalf("truncated block at %d", offset)
		}
		kind := binary.LittleEndian.Uint32(raw[offset:])
		total := int(binary.LittleEndian.Uint32(raw[offset+4:]))
		if total%4 != 0 || offset+total > len(raw) ||
			int(binary.LittleEndian.Uint32(raw[offset+total-4:])) != total {
			t.Fatalf("bad block length %d at %d", total, offset)
		}
		block := raw[offset : offset+total]
		switch {
		case offset == 0 && kind != 0x0A0D0D0A, offset == 28 && kind != 1:
			t.Fatalf("block %d has type %#x", offset, kind)
		case kind == 6:
			captured := int(binary.LittleEndian.Uint32(block[20:]))
			packet := devPcapPacket{
				data:     block[28 : 28+captured],
				original: int(binary.LittleEndian.Uint32(block[24:])),
			}
			options := block[28+((captured+3)&^3) : total-4]
			for len(options) >= 4 {
				code := binary.LittleEndian.Uint16(options)
				length := int(binary.LittleEndian.Uint16(options[2:]))
				if code == 0 {
					break
				}
				if code == 1 {
					packet.comment = string(options[4 : 4+length])
				}
				options = options[4+((length+3)&^3):]
			}
			packets = append(packets, packet)
		}
		offset += total
	}
	return packets
}

func devPcapFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "ims-inner-*.pcapng"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestDevPcapRecordsSAOutcomeWithoutPayload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(devPcapDirEnv, dir)
	policy := testIPSecPolicy()
	ue, _ := newProtectedUDPNetwork(t, policy.LocalIP)
	server, endpoint := newProtectedUDPNetwork(t, policy.RemoteIP)
	destination := &net.UDPAddr{IP: policy.LocalIP, Port: int(policy.LocalServerPort)}
	receiver, err := ue.ListenProtectedUDP(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := server.ListenPacket("udp", &net.UDPAddr{IP: policy.RemoteIP, Port: int(policy.RemoteClientPort)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	plain := writeProtectedUDPTestPacket(t, sender, destination, endpoint)
	_ = ue.network.bridge.injectInboundPacket(plain)
	if err := ue.InstallIPSec3GPP(policy); err != nil {
		t.Fatal(err)
	}
	if err := server.InstallIPSec3GPP(reverseProtectedTestPolicy(policy)); err != nil {
		t.Fatal(err)
	}
	wire := writeProtectedUDPTestPacket(t, sender, destination, endpoint)
	if err := ue.network.bridge.injectInboundPacket(wire); err != nil {
		t.Fatal(err)
	}
	_ = ue.network.bridge.injectInboundPacket(wire)
	_ = ue.network.Close()
	_ = server.network.Close()

	files := devPcapFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("pcap files = %v, want one per network", files)
	}
	var comments []string
	for _, file := range files {
		raw, _ := os.ReadFile(file)
		if bytes.Contains(raw, []byte("protected test")) {
			t.Fatalf("%s carries UDP payload", file)
		}
		for _, packet := range readDevPcap(t, file) {
			if len(packet.data) >= packet.original && packet.original > 48 {
				t.Errorf("packet kept %d of %d bytes: %q", len(packet.data), packet.original, packet.comment)
			}
			comments = append(comments, packet.comment)
		}
	}
	joined := strings.Join(comments, "\n")
	for _, want := range []string{
		"in drop: plaintext udp on reserved IMS port",
		"out esp spi=" + itoa(policy.LocalServerSPI) + " seq=",
		"in esp spi=" + itoa(policy.LocalServerSPI) + " seq=1",
		"in drop esp spi=" + itoa(policy.LocalServerSPI) + " seq=1: ipsec3gpp: replay packet rejected",
		"len=14",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in comments:\n%s", want, joined)
		}
	}
}

func itoa(value uint32) string { return strconv.FormatUint(uint64(value), 10) }

func TestDevPayloadSummaryKeepsOnlyMethodStatusAndCSeq(t *testing.T) {
	request := "NOTIFY sip:262021234567890@[2001:db8::2]:5062;transport=tcp SIP/2.0\r\n" +
		"From: <sip:+4915112345678@ims.example>\r\nCSeq: 1 NOTIFY\r\n\r\nbody"
	got := devPayloadSummary([]byte(request))
	if got != "len="+strconv.Itoa(len(request))+" sip=NOTIFY cseq=1_NOTIFY" {
		t.Fatalf("request summary = %q", got)
	}
	if got := devPayloadSummary([]byte("SIP/2.0 200 OK\r\ncseq: 3 REGISTER\r\n\r\n")); !strings.HasSuffix(got, " sip=200 cseq=3_REGISTER") {
		t.Fatalf("response summary = %q", got)
	}
	if got := devPayloadSummary([]byte("\r\n\r\n")); got != "len=4 crlf-ping" {
		t.Fatalf("ping summary = %q", got)
	}
	// A segment that starts mid-message reveals nothing but its length.
	if got := devPayloadSummary([]byte("sip:262021234567890@ims.example\r\n")); got != "len=33" {
		t.Fatalf("continuation summary = %q", got)
	}
}

func devTestIPv6(next byte, rest []byte) []byte {
	packet := make([]byte, 40, 40+len(rest))
	packet[0], packet[6] = 0x60, next
	binary.BigEndian.PutUint16(packet[4:], uint16(len(rest)))
	return append(packet, rest...)
}

func devTestIPv4(protocol byte, fragmentOffset uint16, rest []byte) []byte {
	packet := make([]byte, 20, 20+len(rest))
	packet[0], packet[9] = 0x45, protocol
	binary.BigEndian.PutUint16(packet[2:], uint16(20+len(rest)))
	binary.BigEndian.PutUint16(packet[6:], fragmentOffset)
	return append(packet, rest...)
}

func TestDevPcapCutKeepsNoPayload(t *testing.T) {
	secret := []byte("NOTIFY sip:+4915112345678@ims.example SIP/2.0\r\nimei:35000011112222\r\n")
	tcp := make([]byte, 32)
	tcp[12] = 8 << 4 // 32-byte TCP header with options
	udp := make([]byte, 8)
	destinationOptions := []byte{6, 0, 1, 4, 0, 0, 0, 0} // next header TCP, 8 bytes
	fragment := func(next byte, offset uint16) []byte {
		header := []byte{next, 0, 0, 0, 0, 0, 0, 1}
		binary.BigEndian.PutUint16(header[2:], offset<<3)
		return header
	}
	echo := append([]byte{128, 0, 0, 0, 0, 1, 0, 1}, secret...)
	unreachable := append([]byte{1, 4, 0, 0, 0, 0, 0, 0}, devTestIPv6(17, append(udp, secret...))...)
	cases := []struct {
		name   string
		packet []byte
		keep   int
	}{
		{"IPv6 TCP", devTestIPv6(6, append(tcp, secret...)), 40 + 32},
		{"IPv6 UDP", devTestIPv6(17, append(udp, secret...)), 40 + 8},
		{"IPv6 destination options then TCP", devTestIPv6(60, append(append(destinationOptions, tcp...), secret...)), 40 + 8 + 32},
		{"IPv6 first fragment", devTestIPv6(44, append(append(fragment(6, 0), tcp...), secret...)), 40 + 8 + 32},
		{"IPv6 later fragment", devTestIPv6(44, append(fragment(6, 100), secret...)), 40 + 8},
		{"IPv4 later fragment", devTestIPv4(6, 100, secret), 20},
		{"ICMPv6 echo", devTestIPv6(58, echo), 40 + 8},
		{"ICMPv6 error quoting UDP", devTestIPv6(58, unreachable), 40 + 8 + 40 + 8},
		{"ICMPv4 echo", devTestIPv4(1, 0, append([]byte{8, 0, 0, 0, 0, 1, 0, 1}, secret...)), 20 + 8},
		{"unknown protocol", devTestIPv6(99, secret), 40},
		{"not IP", secret, 0},
		{"IPv6 length mismatch", append(devTestIPv6(6, tcp), secret...), 0},
	}
	for _, tc := range cases {
		keep, summary := devPcapCut(tc.packet)
		if keep != tc.keep {
			t.Errorf("%s: kept %d bytes, want %d", tc.name, keep, tc.keep)
		}
		for _, leak := range []string{"4915112345678", "35000011112222", "ims.example"} {
			if bytes.Contains(tc.packet[:keep], []byte(leak)) || strings.Contains(summary, leak) {
				t.Errorf("%s: kept %q (summary %q)", tc.name, leak, summary)
			}
		}
	}
}

func TestDevPcapRecordAfterCloseIsDropped(t *testing.T) {
	t.Setenv(devPcapDirEnv, t.TempDir())
	p := openDevPcap()
	p.Close()
	p.record(time.Now(), devTestIPv6(17, make([]byte, 8)), "in plaintext")
	p.Close()
}

func TestDevPcapRotatesAndKeepsTheNewestFiles(t *testing.T) {
	dir := t.TempDir()
	file, path, written, err := createDevPcapFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := &devPcap{dir: dir, path: path, file: file, written: written}
	for i := 0; i < devPcapKeepFiles+5; i++ {
		if !p.rotate() {
			t.Fatalf("rotation %d failed", i)
		}
	}
	t.Cleanup(func() {
		_ = p.file.Close()
		devPcapReleased(p.path)
	})
	names, _ := filepath.Glob(filepath.Join(dir, "ims-inner-*.pcapng"))
	if len(names) != devPcapKeepFiles {
		t.Fatalf("kept %d files, want %d", len(names), devPcapKeepFiles)
	}
	// Another tunnel's older file that is still open is never pruned.
	devPcapActiveMu.Lock()
	devPcapActive[names[0]] = true
	devPcapActiveMu.Unlock()
	other, otherPath, _, err := createDevPcapFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = other.Close()
		devPcapReleased(otherPath)
	})
	for i := 0; i < 3; i++ {
		p.rotate()
	}
	if _, err := os.Stat(names[0]); err != nil {
		t.Fatalf("an open capture was pruned: %v", err)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatalf("the other tunnel's current capture was pruned: %v", err)
	}
	devPcapReleased(names[0])
	if _, err := os.Stat(p.path); err != nil || p.full.Load() {
		t.Fatalf("current file %s missing or capture stopped: %v", p.path, err)
	}
}

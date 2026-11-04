package netstack

// [dev] SIP trace of every IMS TCP connection and UDP socket the netstack
// hands out: each complete SIP message in either direction is logged once,
// redacted by logging.DevRedactSIP. TCP connections also log when they open,
// when the first read fails (EOF, RST, timeout) and when they close, with byte
// counts and how long each direction had been quiet. Debug-field branch only.

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

const devSIPStreamLimit = 256 << 10

var (
	devConnSeq          atomic.Uint64
	devSIPStartLine     = regexp.MustCompile(`^(SIP/2\.0 [0-9]{3}|[A-Z]{3,10} \S+ SIP/2\.0)`)
	devContentLengthHdr = regexp.MustCompile(`(?im)^(?:content-length|l)[ \t]*:[ \t]*([0-9]+)[ \t]*\r?$`)
)

type devConnTrace struct {
	id     uint64
	proto  string
	origin string
	local  string
	remote string
	opened time.Time

	txBytes, rxBytes atomic.Uint64
	lastTx, lastRx   atomic.Int64
	nonSIP           atomic.Uint64
	readFailed       atomic.Bool
	closed           atomic.Bool

	txMu, rxMu sync.Mutex
	tx, rx     []byte
}

func newDevConnTrace(proto, origin string, local, remote net.Addr) *devConnTrace {
	trace := &devConnTrace{
		id: devConnSeq.Add(1), proto: proto, origin: origin,
		local: devAddr(local), remote: devAddr(remote), opened: time.Now(),
	}
	logging.DevLog("IMS conn opened", "conn", trace.id, "proto", proto, "origin", origin,
		"local", trace.local, "remote", trace.remote)
	return trace
}

func devAddr(address net.Addr) string {
	if address == nil {
		return ""
	}
	return address.String()
}

// stream feeds one TCP direction and logs every complete SIP message in it.
func (t *devConnTrace) stream(direction string, data []byte) {
	if t == nil || len(data) == 0 {
		return
	}
	mu, buffer := &t.rxMu, &t.rx
	if direction == "tx" {
		mu, buffer = &t.txMu, &t.tx
	}
	mu.Lock()
	defer mu.Unlock()
	*buffer = append(*buffer, data...)
	for {
		// RFC 5626 keepalives: CRLFCRLF ping, CRLF pong.
		trimmed := bytes.TrimLeft(*buffer, "\r\n")
		if skipped := len(*buffer) - len(trimmed); skipped > 0 {
			if count := t.nonSIP.Add(1); logging.DevSampled(count) {
				logging.DevLog("IMS conn keepalive", "conn", t.id, "dir", direction, "bytes", skipped, "count", count)
			}
			*buffer = trimmed
		}
		if len(*buffer) == 0 {
			*buffer = nil
			return
		}
		size, ok := devSIPMessageSize(*buffer)
		if !ok {
			if len(*buffer) > devSIPStreamLimit {
				logging.DevLog("IMS conn stream not parsable as SIP; buffer dropped", "conn", t.id,
					"dir", direction, "bytes", len(*buffer), "head", strconv.Quote(devHead(*buffer)))
				*buffer = nil
			}
			return
		}
		t.logSIP(direction, string((*buffer)[:size]))
		*buffer = (*buffer)[size:]
	}
}

// devSIPMessageSize returns the length of the first complete message, using
// Content-Length (RFC 3261 §18.3) and 0 when a stream message omits it.
func devSIPMessageSize(buffer []byte) (int, bool) {
	end, separator := bytes.Index(buffer, []byte("\r\n\r\n")), 4
	if bare := bytes.Index(buffer, []byte("\n\n")); end < 0 || (bare >= 0 && bare < end) {
		end, separator = bare, 2
	}
	if end < 0 {
		return 0, false
	}
	length := 0
	if match := devContentLengthHdr.FindSubmatch(buffer[:end]); match != nil {
		length, _ = strconv.Atoi(string(match[1]))
	}
	size := end + separator + length
	return size, size <= len(buffer)
}

func (t *devConnTrace) logSIP(direction, message string) {
	firstLine, _, _ := strings.Cut(message, "\n")
	firstLine = strings.TrimSpace(firstLine)
	if !devSIPStartLine.MatchString(firstLine) {
		if count := t.nonSIP.Add(1); logging.DevSampled(count) {
			logging.DevLog("IMS conn non-SIP data", "conn", t.id, "proto", t.proto, "dir", direction,
				"bytes", len(message), "count", count, "head", strconv.Quote(devHead([]byte(message))))
		}
		return
	}
	logging.DevLog("IMS SIP "+direction, "conn", t.id, "proto", t.proto, "origin", t.origin,
		"local", t.local, "remote", t.remote, "bytes", len(message),
		"first_line", logging.DevRedactSIP(firstLine), "sip", logging.DevRedactSIP(message))
}

func devHead(data []byte) string {
	if len(data) > 48 {
		data = data[:48]
	}
	return logging.RedactSIPRaw(string(data))
}

func (t *devConnTrace) wrote(data []byte, n int) {
	if t == nil || n <= 0 {
		return
	}
	t.txBytes.Add(uint64(n))
	t.lastTx.Store(time.Now().UnixNano())
	if t.proto == "tcp" {
		t.stream("tx", data[:n])
	}
}

func (t *devConnTrace) read(data []byte, n int, err error) {
	if t == nil {
		return
	}
	if n > 0 {
		t.rxBytes.Add(uint64(n))
		t.lastRx.Store(time.Now().UnixNano())
		if t.proto == "tcp" {
			t.stream("rx", data[:n])
		}
	}
	if err != nil && !t.closed.Load() && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrDeadlineExceeded) &&
		!t.readFailed.Swap(true) {
		logging.DevLog("IMS conn read failed", append(t.stats(), "eof", errors.Is(err, io.EOF), "err", err)...)
	}
}

func (t *devConnTrace) close(err error) {
	if t == nil || t.closed.Swap(true) {
		return
	}
	logging.DevLog("IMS conn closed", append(t.stats(), "err", err)...)
}

func (t *devConnTrace) stats() []any {
	now := time.Now()
	since := func(at int64) string {
		if at == 0 {
			return "never"
		}
		return now.Sub(time.Unix(0, at)).Round(time.Millisecond).String()
	}
	t.rxMu.Lock()
	pendingRx := len(t.rx)
	t.rxMu.Unlock()
	return []any{"conn", t.id, "proto", t.proto, "origin", t.origin, "local", t.local, "remote", t.remote,
		"age", now.Sub(t.opened).Round(time.Millisecond).String(),
		"tx_bytes", t.txBytes.Load(), "rx_bytes", t.rxBytes.Load(),
		"since_tx", since(t.lastTx.Load()), "since_rx", since(t.lastRx.Load()), "pending_rx_bytes", pendingRx}
}

// devUDPConn traces each SIP datagram of an IMS UDP socket; RTP, STUN and
// other non-SIP datagrams are only counted, sampled.
type devUDPConn struct {
	*gonet.UDPConn
	trace *devConnTrace
}

func newDevUDPConn(conn *gonet.UDPConn, origin string) *devUDPConn {
	return &devUDPConn{UDPConn: conn, trace: newDevConnTrace("udp", origin, conn.LocalAddr(), conn.RemoteAddr())}
}

func (c *devUDPConn) datagram(direction string, data []byte, peer net.Addr) {
	if len(data) == 0 {
		return
	}
	if !devSIPStartLine.Match(data) {
		if count := c.trace.nonSIP.Add(1); logging.DevSampled(count) {
			logging.DevLog("IMS UDP non-SIP datagram", "conn", c.trace.id, "dir", direction,
				"peer", devAddr(peer), "bytes", len(data), "count", count)
		}
		return
	}
	message := string(data)
	firstLine, _, _ := strings.Cut(message, "\n")
	logging.DevLog("IMS SIP "+direction, "conn", c.trace.id, "proto", "udp", "origin", c.trace.origin,
		"local", c.trace.local, "remote", devAddr(peer), "bytes", len(data),
		"first_line", logging.DevRedactSIP(strings.TrimSpace(firstLine)), "sip", logging.DevRedactSIP(message))
}

func (c *devUDPConn) Read(b []byte) (int, error) {
	n, err := c.UDPConn.Read(b)
	if n > 0 {
		c.datagram("rx", b[:n], c.UDPConn.RemoteAddr())
	}
	return n, err
}

func (c *devUDPConn) Write(b []byte) (int, error) {
	n, err := c.UDPConn.Write(b)
	if n > 0 {
		c.datagram("tx", b[:n], c.UDPConn.RemoteAddr())
	}
	return n, err
}

func (c *devUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, peer, err := c.UDPConn.ReadFrom(b)
	if n > 0 {
		c.datagram("rx", b[:n], peer)
	}
	return n, peer, err
}

func (c *devUDPConn) WriteTo(b []byte, peer net.Addr) (int, error) {
	n, err := c.UDPConn.WriteTo(b, peer)
	if n > 0 {
		c.datagram("tx", b[:n], peer)
	}
	return n, err
}

func (c *devUDPConn) Close() error {
	err := c.UDPConn.Close()
	c.trace.close(err)
	return err
}

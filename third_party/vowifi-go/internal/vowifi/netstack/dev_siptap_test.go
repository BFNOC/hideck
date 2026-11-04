package netstack

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func captureDevLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func TestDevSIPStreamSplitsMessagesAcrossReads(t *testing.T) {
	output := captureDevLog(t)
	trace := &devConnTrace{id: 7, proto: "tcp"}
	options := "OPTIONS sip:u@[2a00::1]:6000 SIP/2.0\r\nCSeq: 1 OPTIONS\r\nContent-Length: 0\r\n\r\n"
	message := "MESSAGE sip:u@[2a00::1]:6000 SIP/2.0\r\nCSeq: 2 MESSAGE\r\n" +
		"Content-Type: application/vnd.3gpp.sms\r\nContent-Length: 11\r\n\r\nsecret-body"
	wire := "\r\n\r\n" + options + message + "\r\n"
	for _, chunk := range []string{wire[:10], wire[10:90], wire[90 : len(wire)-6], wire[len(wire)-6:]} {
		trace.stream("rx", []byte(chunk))
	}
	log := output.String()
	if strings.Count(log, `msg="[dev] IMS SIP rx"`) != 2 || !strings.Contains(log, "CSeq: 1 OPTIONS") ||
		!strings.Contains(log, "CSeq: 2 MESSAGE") {
		t.Fatalf("log = %s", log)
	}
	if strings.Contains(log, "secret-body") || !strings.Contains(log, "body omitted") {
		t.Fatalf("SMS body reached the log: %s", log)
	}
	if !strings.Contains(log, "IMS conn keepalive") || len(trace.rx) != 0 {
		t.Fatalf("keepalive not consumed: rx=%q log=%s", trace.rx, log)
	}
}

func TestDevSIPMessageSizeWaitsForTheBody(t *testing.T) {
	head := "SIP/2.0 200 OK\r\nl: 5\r\n\r\n"
	if _, ok := devSIPMessageSize([]byte(head + "abc")); ok {
		t.Fatal("incomplete body reported complete")
	}
	if size, ok := devSIPMessageSize([]byte(head + "abcdeNEXT")); !ok || size != len(head)+5 {
		t.Fatalf("size = %d, %t", size, ok)
	}
}

func TestDevConnTraceLogsReadFailureOnce(t *testing.T) {
	output := captureDevLog(t)
	trace := &devConnTrace{id: 3, proto: "tcp"}
	trace.read(nil, 0, errTestReset)
	trace.read(nil, 0, errTestReset)
	trace.close(nil)
	trace.close(nil)
	log := output.String()
	if strings.Count(log, "IMS conn read failed") != 1 || strings.Count(log, "IMS conn closed") != 1 ||
		!strings.Contains(log, "since_rx=never") {
		t.Fatalf("log = %s", log)
	}
}

type testError string

func (e testError) Error() string { return string(e) }

const errTestReset = testError("connection reset by peer")

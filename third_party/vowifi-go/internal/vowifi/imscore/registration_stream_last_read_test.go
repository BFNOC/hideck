package imscore

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

func TestRegistrationStreamCloseReportsSilenceAndKeepsCause(t *testing.T) {
	local, remote := net.Pipe()
	go func() {
		_, _ = remote.Write([]byte("\r\n"))
		_ = remote.Close()
	}()
	err := (&Service{cfg: &IMSConfig{}}).readRegistrationStreamSync(local)
	if !errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "(last read 0s ago)") {
		t.Fatalf("stream close error = %v", err)
	}
}

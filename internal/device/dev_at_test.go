package device

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yibaiba/hideck/pkg/logger"
)

func TestDevLogATKeepsOnlyTheCommandNameAndResult(t *testing.T) {
	logger.Setup(logger.LogConfig{Debug: true, Filename: filepath.Join(t.TempDir(), "app.log")})
	logs := logger.GlobalBroadcaster.Subscribe()
	defer logger.GlobalBroadcaster.Unsubscribe(logs)
	devLogAT("wwan0", `AT+QADBKEY="12345678"`, "+QADBKEY: 262021234567897\r\n+CME ERROR: 3\r\n", errors.New("port /dev/ttyUSB2 imei 860349050869734"), time.Second)
	entry := <-logs
	if strings.Contains(entry.Fields, "12345678") || strings.Contains(entry.Fields, "262021") ||
		strings.Contains(entry.Fields, "86034905") || !strings.Contains(entry.Fields, `"command":"AT+QADBKEY"`) ||
		!strings.Contains(entry.Fields, `"result":"+CME ERROR"`) {
		t.Fatalf("fields = %s", entry.Fields)
	}
}

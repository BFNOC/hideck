package device

// [dev] debug-field: every AT command run through the Pool's AT gate, with
// its response (11+ digit runs masked: IMSI, ICCID, IMEI) and timing.

import (
	"regexp"
	"strings"
	"time"

	"github.com/yibaiba/hideck/pkg/logger"
)

var devATLongDigits = regexp.MustCompile(`[0-9]{11,}`)

func devLogAT(deviceID, command, response string, err error, elapsed time.Duration) {
	logger.Info("[dev] AT command", "device", deviceID, "command", devATLongDigits.ReplaceAllString(command, "<masked>"),
		"response", devATLongDigits.ReplaceAllString(strings.TrimSpace(response), "<masked>"),
		"elapsed", elapsed.String(), "err", err)
}

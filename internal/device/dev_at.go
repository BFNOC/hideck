package device

// [dev] debug-field: every AT command run through the Pool's AT gate, by
// command name only (no arguments) with the final result, response size and
// timing. Responses can carry IMSI, ICCID, keys or SMS, so they are not logged.

import (
	"regexp"
	"strings"
	"time"

	"github.com/yibaiba/hideck/pkg/logger"
)

var (
	// Extended commands keep their name (AT+CFUN); basic ones keep only the
	// letter (ATD, never the dialled number). The suffix shows set, read or test.
	devATCommandName = regexp.MustCompile(`^(?i)AT(?:[+$^%&][A-Z][A-Z0-9]*|[A-Z])(=\?|\?|=)?`)
	devATLongDigits  = regexp.MustCompile(`[0-9]{8,}`)
)

func devLogAT(deviceID, command, response string, err error, elapsed time.Duration) {
	result := "none"
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if line == "OK" || line == "ERROR" || strings.HasPrefix(line, "+CME ERROR") || strings.HasPrefix(line, "+CMS ERROR") {
			result = line
			break
		}
	}
	if strings.HasPrefix(result, "+CM") {
		result = result[:len("+CME ERROR")]
	}
	logger.Info("[dev] AT command", "device", deviceID, "command", devATCommandName.FindString(strings.TrimSpace(command)),
		"result", result, "response_bytes", len(response), "elapsed", elapsed.String(), "err", devATError(err))
}

func devATError(err error) string {
	if err == nil {
		return ""
	}
	return devATLongDigits.ReplaceAllString(err.Error(), "<masked>")
}

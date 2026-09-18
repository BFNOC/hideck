package device

import (
	"strings"

	"github.com/nyaruka/phonenumbers/v2"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

// smsDestinationForRegion only expands a full national number that is valid
// for the sending SIM's home region. Short codes and ambiguous numbers remain
// in the SMSC's national dialing plan.
func smsDestinationForRegion(raw, region string) string {
	to := strings.TrimSpace(raw)
	if to == "" || region == "" || strings.HasPrefix(to, "+") || smscodec.IsShortCode(to) {
		return to
	}
	for _, digit := range to {
		if digit < '0' || digit > '9' {
			return to
		}
	}
	number, err := phonenumbers.Parse(to, region)
	if err != nil || !phonenumbers.IsValidNumberForRegion(number, region) {
		return to
	}
	return phonenumbers.Format(number, phonenumbers.E164)
}

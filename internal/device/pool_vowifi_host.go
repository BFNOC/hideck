package device

import "github.com/yibaiba/hideck/internal/vowifihost"

func (p *Pool) voWiFiHost() *vowifihost.Manager {
	if p == nil {
		return vowifihost.NewManager()
	}
	if p.vowifiHost == nil {
		p.vowifiHost = vowifihost.NewManager()
	}
	return p.vowifiHost
}

// BeginVoWiFiStartForTest claims a VoWiFi start the way a lifecycle enable
// does, for tests in other packages whose lifecycle run is replaced. Disable
// and restart drop the claim when they invalidate the runtime.
func (p *Pool) BeginVoWiFiStartForTest(deviceID string) {
	p.voWiFiHost().BeginStart(deviceID)
}

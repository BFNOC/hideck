package swu

import (
	"net"
	"strconv"

	"github.com/iniwex5/vowifi-go/engine/ipsec"
	"github.com/iniwex5/vowifi-go/engine/logger"
	"go.uber.org/zap"
)

const natTPort = 4500

// floatToNATTPort moves IKE to UDP/4500 after NAT detection (RFC 7296 §2.23).
// Over SOCKS5 it opens a new UDP ASSOCIATE for 4500 instead of re-addressing
// the old one: some relays (VMess upstreams among them) pin an association to
// its first destination and silently drop datagrams sent to another port, so
// IKE_SA_INIT succeeds and IKE_AUTH times out. The relay's source port toward
// the ePDG changes with the association before IKE_AUTH, so the CHILD_SA is
// created on the new address; RFC 7296 §2.11 has the responder accept any
// source port and reply to the address and port the request came from.
// §2.23's "send all subsequent traffic from port 4500" refers to our sending
// port; the SOCKS5 UDP socket is bound to an ephemeral port, not 4500, both
// before and after this change.
// If the new association cannot be opened the old one is kept.
func (s *Session) floatToNATTPort() {
	current := s.transport()
	if proxied, ok := current.(*ipsec.Socks5Transport); ok && proxied.RemotePort() != natTPort {
		if s.reassociateSocks5(proxied) {
			return
		}
	}
	current.SetRemotePort(natTPort)
	s.mu.Lock()
	s.remotePort = natTPort
	s.mu.Unlock()
}

func (s *Session) reassociateSocks5(previous *ipsec.Socks5Transport) bool {
	remote := net.JoinHostPort(previous.RemoteIP().String(), strconv.Itoa(natTPort))
	replacement, err := s.openSocks5Transport(remote)
	if err != nil {
		logger.Warn("SWu NAT-T float kept the existing SOCKS5 UDP association",
			zap.String("remote", remote), zap.Error(err))
		return false
	}
	// A full handshake after a declined session resumption still has the
	// resumption's IKE control loop reading the old transport; move it along.
	controlWasRunning, err := s.stopIKEControlGeneration()
	if err != nil {
		replacement.Stop()
		logger.Warn("SWu NAT-T float kept the existing SOCKS5 UDP association",
			zap.String("remote", remote), zap.Error(err))
		return false
	}
	s.setTransport(replacement)
	s.mu.Lock()
	s.remotePort = natTPort
	s.mu.Unlock()
	s.startNetEventMonitor()
	previous.Stop()
	logger.Info("SWu NAT-T float opened a new SOCKS5 UDP association",
		zap.String("remote", remote),
		zap.Uint16("old_local_port", previous.LocalPort()),
		zap.Uint16("new_local_port", replacement.LocalPort()),
		zap.Bool("ike_control_restarted", controlWasRunning))
	if controlWasRunning {
		if err := s.startIKEControl(); err != nil {
			logger.Warn("SWu NAT-T float could not restart IKE control", zap.Error(err))
		}
	}
	return true
}

func (s *Session) openSocks5Transport(remote string) (*ipsec.Socks5Transport, error) {
	transport, err := ipsec.NewSocks5Transport(s.proxyTransportConfig(remote))
	if err != nil {
		return nil, err
	}
	if err := transport.Start(); err != nil {
		transport.Stop()
		return nil, err
	}
	return transport, nil
}

package netstack

import (
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

func TestConfigureIMSTCPKeepaliveSetsUserTimeout(t *testing.T) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	defer s.Close()
	var queue waiter.Queue
	endpoint, err := s.NewEndpoint(tcp.ProtocolNumber, ipv6.ProtocolNumber, &queue)
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	if err := configureIMSTCPKeepalive(endpoint); err != nil {
		t.Fatal(err)
	}
	var got tcpip.TCPUserTimeoutOption
	if err := endpoint.GetSockOpt(&got); err != nil {
		t.Fatal(err)
	}
	if want := tcpip.TCPUserTimeoutOption(imsTCPUserTimeout); got != want || imsTCPUserTimeout.Seconds() != 45 {
		t.Fatalf("user timeout = %v, want %v (45s)", got, want)
	}
}

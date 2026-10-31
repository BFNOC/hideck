package imscore

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/policy"
)

// TS 24.229 5.1.1.4.1: a 403 to a reregistration is answered with an initial
// registration, not with another reregistration (the fallback template retry).
// Running it in the same tunnel instead of asking for a reconnect, and its new
// Call-ID, are our choices.
func TestRefreshForbiddenRegistersAgainInTheSameTunnel(t *testing.T) {
	registrar, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	defer registrar.Close()
	statuses := []int{200, 403, 200}
	requests := make(chan string, len(statuses)+1)
	go func() {
		buffer := make([]byte, 64*1024)
		for index := 0; ; index++ {
			n, remote, err := registrar.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			request := string(buffer[:n])
			requests <- request
			status, extra := 500, "Warning: 399 test \"unexpected REGISTER\"\r\n"
			if index < len(statuses) {
				status, extra = statuses[index], "Expires: 3600\r\n"
			}
			if index == 0 {
				extra = "Expires: 1\r\n"
			}
			_, _ = registrar.WriteToUDP([]byte(registerWireResponse(request, status, extra)), remote)
		}
	}()
	template := policy.DefaultIMSRegisterTemplate()
	if !template.EnableInitialRejectFallback {
		t.Fatal("test needs a template with the initial reject fallback")
	}
	svc, err := New(&IMSConfig{
		DeviceID: "dev-forbidden", IMSI: "310260123456789", IMPI: "310260123456789@ims.example",
		IMPU: "sip:310260123456789@ims.example", Domain: "ims.example",
		LocalIP: net.IPv4(127, 0, 0, 1), Transport: "udp", Registrar: registrar.LocalAddr().String(),
		IMSNetwork: NewSystemIMSNetwork(net.IPv4(127, 0, 0, 1)), AKAProvider: stubAKAProvider{},
		EnableIPSec3GPP: disabledBoolPointer(), IMSRegisterTemplate: template,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer svc.StopCurrent()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	seen := make([]string, 0, len(statuses))
	for len(seen) < len(statuses) {
		select {
		case request := <-requests:
			seen = append(seen, request)
		case <-ctx.Done():
			t.Fatalf("received %d REGISTER requests before timeout", len(seen))
		}
	}
	if sipHeaderValue(seen[0], "Call-ID") != sipHeaderValue(seen[1], "Call-ID") {
		t.Fatal("refresh changed Call-ID")
	}
	if sipHeaderValue(seen[2], "Call-ID") == sipHeaderValue(seen[1], "Call-ID") {
		t.Fatal("403 to the refresh was retried as another reregistration")
	}
	deadline := time.Now().Add(2 * time.Second)
	for svc.registrationGeneration.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if svc.registrationGeneration.Load() < 2 || !svc.IsRegistered() {
		t.Fatalf("initial registration after 403 did not complete: state %q", svc.RegState())
	}
	select {
	case err := <-svc.RegistrationErrors():
		t.Fatalf("403 asked for a reconnect: %v", err)
	case extra := <-requests:
		t.Fatalf("unexpected extra REGISTER after recovery: %q", sipHeaderValue(extra, "CSeq"))
	case <-time.After(200 * time.Millisecond):
	}
}

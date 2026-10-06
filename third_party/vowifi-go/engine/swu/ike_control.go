package swu

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/iniwex5/vowifi-go/engine/ikev2"
	"github.com/iniwex5/vowifi-go/engine/ipsec"
	"github.com/iniwex5/vowifi-go/engine/logger"
	"go.uber.org/zap"
)

const (
	defaultIKEWindowSize = 5
	ikeControlQueueSize  = 8
)

type ikeWaitKey struct {
	exchangeType ikev2.ExchangeType
	msgID        uint32
}

// startIKEControl starts the single IKE socket reader and peer-request worker.
func (s *Session) startIKEControl() error {
	transport := s.transport()
	if transport == nil {
		return errors.New("swu: no IKE transport")
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.controlMu.Lock()
	if s.controlStopping {
		s.controlMu.Unlock()
		return errors.New("swu: IKE control plane is stopping")
	}
	if s.controlRunning {
		s.controlMu.Unlock()
		return nil
	}
	requests := make(chan []byte, ikeControlQueueSize)
	stop := make(chan struct{})
	taskManager := s.newSessionTaskManager(transport)
	s.controlRequests = requests
	s.controlStop = stop
	s.controlTransport = transport
	s.taskMgr = taskManager
	s.controlRunning = true
	s.controlWG.Add(2)
	s.controlMu.Unlock()
	go s.ikeDispatchLoop()
	go s.ikeRequestLoop(requests)
	return nil
}

func (s *Session) newSessionTaskManager(transport ipsec.Transport) *TaskManager {
	config := s.cfg.IKERetryConfig
	interval := taskManagerPollInterval
	if config == nil && s.cfg.Retransmit != nil {
		config, interval = convertRetransmitConfig(s.cfg.Retransmit)
	}
	return newTaskManager(
		s.ctx, s.cfg.DeviceID, config, defaultIKEWindowSize,
		func(packets [][]byte) error { return s.sendIKEPacketSet(transport, packets) },
		nil, interval,
	)
}

func (s *Session) sendIKEPacketSet(transport ipsec.Transport, packets [][]byte) error {
	if err := sendIKEPacketSet(transport, packets); err != nil {
		return err
	}
	s.markOutboundActivity()
	return nil
}

func sendIKEPacketSet(transport ipsec.Transport, packets [][]byte) error {
	if transport == nil {
		return errors.New("swu: no IKE transport")
	}
	if len(packets) == 0 {
		return errors.New("swu: empty IKE packet set")
	}
	for _, packet := range packets {
		if len(packet) == 0 {
			return errors.New("swu: empty IKE packet")
		}
		if err := transport.SendIKE(packet); err != nil {
			return fmt.Errorf("swu: send IKE packet set: %w", err)
		}
	}
	return nil
}

// ensureIKEDispatcher restores the original idempotent dispatcher entrypoint.
func (s *Session) ensureIKEDispatcher() {
	if err := s.startIKEControl(); err != nil {
		s.failEstablishedControl(err)
	}
}

// startIKEControlLoop restores the original established-control entrypoint.
func (s *Session) startIKEControlLoop() {
	s.ensureIKEDispatcher()
}

// ikeDispatchLoop restores the original no-argument socket-reader symbol.
func (s *Session) ikeDispatchLoop() {
	s.controlMu.RLock()
	transport := s.controlTransport
	taskManager := s.taskMgr
	requests := s.controlRequests
	stop := s.controlStop
	s.controlMu.RUnlock()
	s.runIKEDispatchLoop(transport, taskManager, requests, stop)
}

func (s *Session) runIKEDispatchLoop(
	transport ipsec.Transport,
	taskManager *TaskManager,
	requests chan<- []byte,
	stop <-chan struct{},
) {
	defer s.controlWG.Done()
	defer taskManager.Stop()
	defer close(requests)
	defer func() {
		s.controlMu.Lock()
		if s.taskMgr == taskManager {
			s.controlRunning = false
			s.controlTransport = nil
		}
		s.controlMu.Unlock()
	}()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-stop:
			return
		case raw, ok := <-transport.IKEPackets():
			if !ok {
				s.failEstablishedControl(errors.New("swu: IKE transport closed"))
				return
			}
			s.markInboundActivity()
			if err := s.dispatchIKEPacket(raw, taskManager, requests); err != nil {
				if s.ctx.Err() != nil {
					s.failEstablishedControl(err)
					return
				}
				// RFC 7296 2.21.4 / RFC 7383 2.6.2: a malformed, unauthenticated,
				// or unknown-SPI datagram is discarded; it must not end the SA.
				logger.Warn("SWu dropped inbound IKE packet", zap.Error(err))
			}
		}
	}
}

func (s *Session) dispatchIKEPacket(
	raw []byte,
	taskManager *TaskManager,
	requests chan<- []byte,
) error {
	header, err := ikev2.DecodeHeader(raw)
	if err != nil {
		return fmt.Errorf("swu: decode established IKE header: %w", err)
	}
	if err := s.validateEstablishedIKEEnvelope(header, raw); err != nil {
		return err
	}
	if handled, err := s.resendRetiredIKEDelete(raw); handled || err != nil {
		return err
	}
	normalized, complete, err := s.normalizeInboundIKE(raw)
	if err != nil {
		return fmt.Errorf("swu: process inbound IKE fragment: %w", err)
	}
	if !complete {
		return nil
	}
	raw = normalized
	header, err = ikev2.DecodeHeader(raw)
	if err != nil {
		return fmt.Errorf("swu: decode normalized IKE header: %w", err)
	}
	if header.Flags&ikeResponseFlag != 0 {
		return s.dispatchIKEResponse(header, raw, taskManager)
	}
	select {
	case requests <- append([]byte(nil), raw...):
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *Session) validateEstablishedIKEEnvelope(header *ikev2.IKEHeader, raw []byte) error {
	if header.Version>>4 != 2 {
		return fmt.Errorf("swu: unsupported IKE version 0x%02x", header.Version)
	}
	if header.Length != uint32(len(raw)) {
		return fmt.Errorf("swu: IKE length %d does not match datagram %d", header.Length, len(raw))
	}
	if s.validBootstrapIKEResponse(header) {
		return nil
	}
	context, retired, err := s.ikeContextForHeader(header)
	if err != nil {
		if s.matchesRetiredIKEDelete(raw) {
			return nil
		}
		return err
	}
	if retired && header.ExchangeType != ikev2.ExchangeInformational {
		return errors.New("swu: retired IKE SA only accepts INFORMATIONAL cleanup")
	}
	return validateIKEContextRole(context, header)
}

func (s *Session) validBootstrapIKEResponse(header *ikev2.IKEHeader) bool {
	if header == nil || header.Flags&ikeResponseFlag == 0 || header.Flags&ikeInitiatorFlag != 0 ||
		header.MessageID != 0 || header.SPIr == 0 {
		return false
	}
	if header.ExchangeType != ikev2.IKE_SA_INIT && header.ExchangeType != ikev2.IKE_SESSION_RESUME {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.spiR == ([8]byte{}) && s.ikeKeys == nil && header.SPIi == ikeSPIUint64(s.spiI)
}

func (s *Session) dispatchIKEResponse(
	header *ikev2.IKEHeader,
	raw []byte,
	taskManager *TaskManager,
) error {
	if taskManager.handleResponseForExchange(header.MessageID, header.ExchangeType, raw) {
		return nil
	}
	key := ikeWaitKey{exchangeType: header.ExchangeType, msgID: header.MessageID}
	s.controlMu.Lock()
	waiter := s.ikeWaiters[key]
	if waiter == nil {
		s.ikePending[key] = append([]byte(nil), raw...)
		s.controlMu.Unlock()
		return nil
	}
	s.controlMu.Unlock()
	select {
	case waiter <- append([]byte(nil), raw...):
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *Session) ikeRequestLoop(requests <-chan []byte) {
	defer s.controlWG.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case raw, ok := <-requests:
			if !ok {
				return
			}
			if s.peerResponse.replay(s, raw) {
				continue
			}
			if err := s.handleIncomingIKE(raw); err != nil {
				s.failEstablishedControl(err)
				return
			}
		}
	}
}

func (s *Session) handleIncomingIKE(raw []byte) error {
	header, err := ikev2.DecodeHeader(raw)
	if err != nil {
		return fmt.Errorf("swu: decode peer IKE header: %w", err)
	}
	switch header.ExchangeType {
	case ikev2.ExchangeInformational:
		return s.handleIncomingInformational(raw)
	case ikev2.ExchangeCreateChildSA:
		s.dispatchCreateChildSA(raw)
		return nil
	default:
		return fmt.Errorf("swu: unsupported established IKE exchange %d", header.ExchangeType)
	}
}

// handleIncomingInformational restores the original raw-packet boundary.
func (s *Session) handleIncomingInformational(raw []byte) error {
	packet, err := ikev2.DecodePacket(raw)
	if err != nil {
		return fmt.Errorf("swu: decode peer INFORMATIONAL: %w", err)
	}
	return s.handleIncomingInformationalPacket(packet)
}

// dispatchCreateChildSA restores the original raw CREATE_CHILD_SA dispatcher.
func (s *Session) dispatchCreateChildSA(raw []byte) {
	packet, err := ikev2.DecodePacket(raw)
	if err == nil {
		err = s.handleIncomingCreateChildSAPacket(packet)
	}
	if err != nil {
		s.failEstablishedControl(fmt.Errorf("swu: handle peer CREATE_CHILD_SA: %w", err))
	}
}

func (s *Session) failEstablishedControl(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	s.stopTimers()
	s.failSession(err)
}

// peerResponseCache implements RFC 7296 2.1: a retransmitted peer request gets
// the same response again instead of being processed a second time, and an
// older request on the same IKE SA is ignored.
type peerResponseCache struct {
	mu         sync.Mutex
	spiI, spiR uint64
	msgID      uint32
	packets    [][]byte
}

func (c *peerResponseCache) record(spiI, spiR [8]byte, msgID uint32, packets [][]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spiI, c.spiR, c.msgID = ikeSPIUint64(spiI), ikeSPIUint64(spiR), msgID
	c.packets = clonePacketSet(packets)
}

// replay reports whether raw was a retransmitted or stale peer request that
// has been answered from the cache (or dropped) and must not be processed.
func (c *peerResponseCache) replay(s *Session, raw []byte) bool {
	header, err := ikev2.DecodeHeader(raw)
	if err != nil {
		return false
	}
	c.mu.Lock()
	sameSA := c.packets != nil && header.SPIi == c.spiI && header.SPIr == c.spiR
	retransmit := sameSA && header.MessageID == c.msgID
	stale := sameSA && header.MessageID < c.msgID
	packets := clonePacketSet(c.packets)
	c.mu.Unlock()
	if retransmit {
		if err := s.sendIKEPacketSet(s.transport(), packets); err != nil {
			logger.Warn("SWu resend cached IKE response failed", zap.Error(err))
		}
	}
	return retransmit || stale
}

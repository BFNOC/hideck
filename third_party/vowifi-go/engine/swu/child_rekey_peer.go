package swu

import (
	"encoding/binary"
	"errors"
	"fmt"

	enginecrypto "github.com/iniwex5/vowifi-go/engine/crypto"
	"github.com/iniwex5/vowifi-go/engine/ikev2"
	"github.com/iniwex5/vowifi-go/engine/ipsec"
	"github.com/iniwex5/vowifi-go/engine/logger"
	"go.uber.org/zap"
)

func (s *Session) handlePeerChildSARekey(packet *ikev2.IKEPacket) error {
	payloads, err := s.decryptAndParse(packet)
	if err != nil {
		return err
	}
	return s.handlePeerChildSARekeyPayloads(packet, payloads)
}

func (s *Session) handlePeerChildSARekeyPayloads(packet *ikev2.IKEPacket, payloads []ikev2.Payload) error {
	if notifyType, err := s.validatePeerRekeyNotify(payloads); err != nil {
		// RFC 7296 2.25.1 and 1.3: refuse with a notify and keep the session.
		s.Logger.Warn("peer CHILD_SA request not accepted; answered with notify",
			zap.Uint16("notify", notifyType), zap.Error(err), zap.String("offer", createChildSAOffer(payloads)))
		return s.sendEstablishedIKEResponse(packet, []ikev2.Payload{
			&ikev2.EncryptedPayloadNotify{NotifyType: notifyType},
		})
	}
	esn := ikev2.AlgorithmType(0)
	if s.espESN {
		esn = 1
	}
	want := map[ikev2.TransformType]ikev2.AlgorithmType{
		ikev2.TransformTypeEncr: ikev2.AlgorithmType(s.espCipher),
		ikev2.TransformTypeDH:   ikev2.AlgorithmType(s.currentChildDHGroup()),
		ikev2.TransformTypeESN:  esn,
	}
	if s.espInteg != 0 {
		want[ikev2.TransformTypeInteg] = ikev2.AlgorithmType(s.espInteg)
	}
	payloads, proposalNum, ok := narrowPeerRekeyOffer(payloads, ikev2.ProtoESP, want, s.espEncKeyBits)
	if !ok {
		return s.rejectPeerRekeyOffer(packet, payloads, errors.New("no proposal matches the current CHILD_SA suite"))
	}
	payloads, ok, err := s.checkPeerRekeyKE(packet, payloads, s.currentChildDHGroup())
	if !ok || err != nil {
		return err
	}
	currentTSi, currentTSr := s.currentChildSelectors()
	peerTSi := retypeTrafficSelectorPayload(currentTSr, ikev2.PayloadTSi)
	peerTSr := retypeTrafficSelectorPayload(currentTSi, ikev2.PayloadTSr)
	selection, err := validateChildSAResponse(payloads, childSAOffer{
		encryption: s.espCipher, encryptionKeyBits: s.espEncKeyBits, integrity: s.espInteg,
		dhGroup: s.currentChildDHGroup(), esn: s.espESN, tsi: peerTSi, tsr: peerTSr,
		requireSA: true, requireNonce: true,
	})
	if err != nil {
		return s.rejectPeerRekeyOffer(packet, payloads, err)
	}
	if !selectorsContainAnyIP(selection.tsr, configuredInnerIPs(s)) {
		return s.rejectPeerRekeyOffer(packet, payloads,
			errors.New("peer CHILD_SA TSr does not contain an assigned inner address"))
	}
	return s.answerPeerChildSARekey(packet, payloads, selection, proposalNum)
}

// narrowPeerRekeyOffer implements the responder side of RFC 7296 2.7: from a
// peer CREATE_CHILD_SA request that may carry several proposals and several
// transforms per type, pick the first proposal for protocol that contains the
// suite this SA already runs and reduce it to one transform per type. The
// returned payloads carry that single proposal renumbered to 1 for validation;
// proposalNum is the peer's original number, which the response MUST echo
// (RFC 7296 3.3.1).
func narrowPeerRekeyOffer(
	payloads []ikev2.Payload,
	protocol ikev2.ProtocolID,
	want map[ikev2.TransformType]ikev2.AlgorithmType,
	keyBits uint16,
) ([]ikev2.Payload, uint8, bool) {
	for index, payload := range payloads {
		sa, ok := payload.(*ikev2.EncryptedPayloadSA)
		if !ok {
			continue
		}
		for _, proposal := range sa.Proposals {
			if proposal == nil || proposal.ProtocolID != protocol {
				continue
			}
			narrowed, ok := narrowPeerProposal(proposal, want, keyBits)
			if !ok {
				continue
			}
			proposalNum := narrowed.ProposalNum
			narrowed.ProposalNum = 1
			out := append([]ikev2.Payload(nil), payloads...)
			out[index] = &ikev2.EncryptedPayloadSA{Proposals: []*ikev2.Proposal{narrowed}}
			return out, proposalNum, true
		}
		return payloads, 0, false
	}
	return payloads, 0, true
}

// narrowPeerProposal accepts a proposal only when every transform type it
// carries has an acceptable entry and every type of the current suite is
// present. D-H NONE stands for no PFS and INTEG NONE for an AEAD cipher; both
// are equivalent to leaving the type out and are dropped (RFC 7296 3.3.3).
func narrowPeerProposal(
	proposal *ikev2.Proposal,
	want map[ikev2.TransformType]ikev2.AlgorithmType,
	keyBits uint16,
) (*ikev2.Proposal, bool) {
	if proposal == nil {
		return nil, false
	}
	narrowed := *proposal
	narrowed.Transforms = nil
	picked := make(map[ikev2.TransformType]bool)
	none := make(map[ikev2.TransformType]bool)
	for _, transform := range proposal.Transforms {
		if transform == nil {
			return nil, false
		}
		id, known := want[transform.Type]
		if transform.ID == 0 && (transform.Type == ikev2.TransformTypeDH && known && id == 0 ||
			transform.Type == ikev2.TransformTypeInteg && !known) {
			none[transform.Type] = true
			continue
		}
		if !known {
			if transform.Type == ikev2.TransformTypeInteg {
				continue
			}
			return nil, false
		}
		if picked[transform.Type] || transform.ID != id || len(transform.Attributes) > 0 && transform.Type != ikev2.TransformTypeEncr {
			continue
		}
		if transform.Type == ikev2.TransformTypeEncr && validateEncryptionKeyLength(transform, keyBits) != nil {
			continue
		}
		narrowed.Transforms = append(narrowed.Transforms, transform)
		picked[transform.Type] = true
	}
	for _, transform := range proposal.Transforms {
		if !picked[transform.Type] && !none[transform.Type] {
			return nil, false
		}
	}
	for transformType, id := range want {
		if !picked[transformType] && !(transformType == ikev2.TransformTypeDH && id == 0) {
			return nil, false
		}
	}
	narrowed.NumTransforms = uint8(len(narrowed.Transforms))
	return &narrowed, len(narrowed.Transforms) > 0
}

// rejectPeerRekeyOffer answers an unacceptable peer rekey with
// NO_PROPOSAL_CHOSEN and keeps the current SA (RFC 7296 2.7, 2.8).
func (s *Session) rejectPeerRekeyOffer(packet *ikev2.IKEPacket, payloads []ikev2.Payload, reason error) error {
	s.Logger.Warn("peer rekey not accepted; answered NO_PROPOSAL_CHOSEN and kept the current SA",
		zap.String("offer", createChildSAOffer(payloads)), zap.Error(reason))
	return s.sendEstablishedIKEResponse(packet, []ikev2.Payload{
		&ikev2.EncryptedPayloadNotify{NotifyType: ikev2.NO_PROPOSAL_CHOSEN},
	})
}

// checkPeerRekeyKE matches the KE payload against the selected D-H group.
// With no group the KE is ignored. A missing or different KE is answered
// with INVALID_KE_PAYLOAD naming the group, keeping the current SA (RFC 7296
// 1.3, 2.7). ok is false when such an answer was sent.
func (s *Session) checkPeerRekeyKE(
	packet *ikev2.IKEPacket,
	payloads []ikev2.Payload,
	group uint16,
) ([]ikev2.Payload, bool, error) {
	if group == 0 {
		kept := make([]ikev2.Payload, 0, len(payloads))
		for _, payload := range payloads {
			if payload == nil || payload.Type() != ikev2.PayloadKE {
				kept = append(kept, payload)
			}
		}
		return kept, true, nil
	}
	got := uint16(0)
	for _, payload := range payloads {
		if payload == nil || payload.Type() != ikev2.PayloadKE {
			continue
		}
		var err error
		got, _, err = parseKEPayload(payload)
		if err == nil && got == group {
			return payloads, true, nil
		}
		break
	}
	s.Logger.Warn("peer rekey KE does not use the selected D-H group; answered INVALID_KE_PAYLOAD",
		zap.Uint16("selected", group), zap.Uint16("ke", got))
	return payloads, false, s.sendEstablishedIKEResponse(packet, []ikev2.Payload{
		&ikev2.EncryptedPayloadNotify{NotifyType: ikev2.INVALID_KE_PAYLOAD, NotifyData: []byte{byte(group >> 8), byte(group)}},
	})
}

func (s *Session) answerPeerChildSARekey(
	packet *ikev2.IKEPacket,
	payloads []ikev2.Payload,
	selection *childSASelection,
	proposalNum uint8,
) error {
	localNonce, localSPI, err := s.newChildSAInitiatorMaterial()
	if err != nil {
		return err
	}
	dh, sharedSecret, err := preparePeerChildRekeyDH(payloads, selection.dhGroup)
	if err != nil {
		return err
	}
	runtime, err := s.prepareChildSARuntime(childSARuntimeSpec{
		localSPI: localSPI, remoteSPI: selection.remoteSPI,
		initiatorNonce: selection.nonce, responderNonce: localNonce,
		sharedSecret: sharedSecret, dh: dh,
		tsi: retypeTrafficSelectorPayload(selection.tsr, ikev2.PayloadTSi),
		tsr: retypeTrafficSelectorPayload(selection.tsi, ikev2.PayloadTSr), localInitiator: false,
	})
	if err != nil {
		if dh != nil {
			enginecrypto.Wipe(dh.SharedKey)
		}
		return err
	}
	proposals := buildESPProposalsForSession(s, localSPI)
	if dh != nil {
		proposals[0].AddTransform(ikev2.TransformTypeDH, ikev2.AlgorithmType(dh.Group), 0)
	}
	if proposalNum != 0 {
		proposals[0].ProposalNum = proposalNum
	}
	response := []ikev2.Payload{
		&ikev2.EncryptedPayloadSA{Proposals: proposals},
		&ikev2.EncryptedPayloadNonce{NonceData: append([]byte(nil), localNonce...)},
	}
	if dh != nil {
		response = append(response, &ikev2.EncryptedPayloadKE{
			DHGroup: ikev2.AlgorithmType(dh.Group), KEData: dh.PublicKeyBytes(),
		})
	}
	response = append(response, cloneTrafficSelectorPayload(selection.tsi), cloneTrafficSelectorPayload(selection.tsr))
	if err := s.sendEstablishedIKEResponse(packet, response); err != nil {
		wipeChildSARuntime(runtime)
		return err
	}
	s.childSAMu.RLock()
	oldLocalSPI, oldRemoteSPI := s.espLocalSPI, s.espRemoteSPI
	s.childSAMu.RUnlock()
	if err := s.activateChildSARuntime(runtime); err != nil {
		return err
	}
	logger.Info("peer CHILD_SA rekey installed",
		zap.Uint32("old_local_spi", oldLocalSPI), zap.Uint32("old_remote_spi", oldRemoteSPI),
		zap.Uint32("new_local_spi", runtime.localSPI), zap.Uint32("new_remote_spi", runtime.remoteSPI))
	s.markChildRekeyComplete()
	s.recordRetiredChildSA(oldRemoteSPI, oldLocalSPI)
	return nil
}

func preparePeerChildRekeyDH(
	payloads []ikev2.Payload,
	group uint16,
) (*enginecrypto.DiffieHellman, []byte, error) {
	if group == 0 {
		_, err := childRekeySharedSecret(payloads, nil)
		return nil, nil, err
	}
	dh, err := enginecrypto.NewDiffieHellman(group)
	if err != nil {
		return nil, nil, err
	}
	if err := dh.GenerateKey(); err != nil {
		return nil, nil, err
	}
	secret, err := childRekeySharedSecret(payloads, dh)
	if err != nil {
		return nil, nil, err
	}
	return dh, secret, nil
}

func retypeTrafficSelectorPayload(payload *ikev2.EncryptedPayloadTS, payloadType ikev2.PayloadType) *ikev2.EncryptedPayloadTS {
	cloned := cloneTrafficSelectorPayload(payload)
	if cloned != nil {
		cloned.IsInitiator = payloadType == ikev2.TSI
	}
	return cloned
}

// validatePeerRekeyNotify checks the REKEY_SA notify of a peer CHILD_SA
// request; on error it also returns the notify type to answer with.
func (s *Session) validatePeerRekeyNotify(payloads []ikev2.Payload) (uint16, error) {
	s.childSAMu.RLock()
	expectedSPI := s.espRemoteSPI
	s.childSAMu.RUnlock()
	for _, payload := range payloads {
		notify, ok := payload.(*ikev2.EncryptedPayloadNotify)
		if !ok || notify.ProtocolID != ikev2.ProtoESP || len(notify.SPI) != 4 ||
			notify.NotifyType != ikev2.NotifyTypeRekeySA {
			continue
		}
		if got := binary.BigEndian.Uint32(notify.SPI); got != expectedSPI {
			return ikev2.CHILD_SA_NOT_FOUND, fmt.Errorf("swu: peer REKEY_SA spi %08x is not the current %08x", got, expectedSPI)
		}
		return 0, nil
	}
	return ikev2.NO_ADDITIONAL_SAS, errors.New("swu: peer CREATE_CHILD_SA without REKEY_SA asks for an additional CHILD_SA")
}

func (s *Session) sendEstablishedIKEResponse(request *ikev2.IKEPacket, payloads []ikev2.Payload) error {
	header := packetIKEHeader(request)
	return s.sendEncryptedResponseWithMsgID(payloads, header.ExchangeType, header.MessageID)
}

func (s *Session) handlePeerInformational(packet *ikev2.IKEPacket) error {
	header := packetIKEHeader(packet)
	context, retiredIKE, err := s.ikeContextForHeader(header)
	if err != nil {
		return err
	}
	if err := validateIKEContextRole(context, header); err != nil {
		return err
	}
	payloads, err := s.decryptAndParseWithKeys(packet, context.keys)
	if err != nil {
		return err
	}
	redirectAddress := s.runtimeRedirectAddress(payloads)
	responsePayloads, err := s.peerMOBIKEResponse(payloads, retiredIKE)
	if err != nil {
		return err
	}
	var activeChildDelete, ikeDelete bool
	s.logDeviceIdentityNotifies("INFORMATIONAL", payloads)
	identityReply, err := s.informationalDeviceIdentityReply(payloads)
	if err != nil {
		return err
	}
	responsePayloads = append(responsePayloads, identityReply...)
	var responseSPIs []uint32
	for _, payload := range payloads {
		deletion, ok := payload.(*ikev2.EncryptedPayloadDelete)
		if !ok {
			continue
		}
		if deletion.ProtocolID == ikev2.ProtoIKE {
			ikeDelete = true
			continue
		}
		if deletion.ProtocolID != ikev2.ProtoESP {
			continue
		}
		activeChildDelete = activeChildDelete || s.deleteContainsCurrentChildSA(deletion.SPIs)
		responseSPIs = append(responseSPIs, s.retireDeletedChildSAs(deletion.SPIs)...)
	}
	responsePayloads = append(responsePayloads, childSADeleteResponse(responseSPIs)...)
	var retiredResponse []byte
	if retiredIKE {
		retiredResponse, err = s.sendIKEContextResponse(packet, responsePayloads, context)
	} else {
		err = s.sendEstablishedIKEResponse(packet, responsePayloads)
	}
	if err != nil {
		return err
	}
	s.handleRuntimeRedirect(redirectAddress)
	if ikeDelete {
		if retiredIKE {
			return s.completeRetiredIKESADelete(context, packet, retiredResponse)
		}
		return errors.New("swu: peer deleted the active IKE_SA")
	}
	if activeChildDelete {
		return errors.New("swu: peer deleted the active CHILD_SA")
	}
	return nil
}

func childSADeleteResponse(spis []uint32) []ikev2.Payload {
	if len(spis) == 0 {
		return nil
	}
	responses := make([]ikev2.Payload, 0, (len(spis)+maxDeleteSPIs-1)/maxDeleteSPIs)
	for start := 0; start < len(spis); start += maxDeleteSPIs {
		end := min(start+maxDeleteSPIs, len(spis))
		encoded := make([]byte, 0, 4*(end-start))
		for _, spi := range spis[start:end] {
			encoded = append(encoded, spiBytes(spi)...)
		}
		responses = append(responses, &ikev2.EncryptedPayloadDelete{
			ProtocolID: ikev2.ProtoESP, SPISize: 4,
			NumSPIs: uint16(end - start), SPIs: encoded,
		})
	}
	return responses
}

func (s *Session) retireDeletedChildSAs(spis []byte) []uint32 {
	var retired []*ipsec.SecurityAssociation
	var retiredLocalSPIs []uint32
	var responseSPIs []uint32
	// RFC 7296 1.4.1: the peer lists its inbound SPIs (our outbound/remote
	// ones); we answer with our inbound halves of the same pairs.
	s.childSAMu.Lock()
	for len(spis) >= 4 {
		remoteSPI := binary.BigEndian.Uint32(spis[:4])
		if localSPI, ok := s.retiredLocalSPIForRemoteLocked(remoteSPI); ok {
			responseSPIs = append(responseSPIs, localSPI)
			retired = append(retired, s.espInboundSAs[localSPI])
			retiredLocalSPIs = append(retiredLocalSPIs, localSPI)
			delete(s.espInboundSAs, localSPI)
			delete(s.retiredChildSAs, localSPI)
		} else if remoteSPI == s.espRemoteSPI {
			responseSPIs = append(responseSPIs, s.espLocalSPI)
		}
		spis = spis[4:]
	}
	s.childSAMu.Unlock()
	for _, association := range retired {
		wipeESPAssociation(association)
	}
	for _, localSPI := range retiredLocalSPIs {
		s.retireKernelInboundChildSA(localSPI)
	}
	return responseSPIs
}

func (s *Session) retiredLocalSPIForRemoteLocked(remoteSPI uint32) (uint32, bool) {
	for localSPI, retiredRemoteSPI := range s.retiredChildSAs {
		if retiredRemoteSPI == remoteSPI {
			return localSPI, true
		}
	}
	return 0, false
}

func (s *Session) deleteContainsCurrentChildSA(spis []byte) bool {
	s.childSAMu.RLock()
	current := s.espRemoteSPI
	s.childSAMu.RUnlock()
	for len(spis) >= 4 {
		if binary.BigEndian.Uint32(spis[:4]) == current {
			return true
		}
		spis = spis[4:]
	}
	return false
}

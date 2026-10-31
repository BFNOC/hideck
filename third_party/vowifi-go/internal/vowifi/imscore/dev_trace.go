package imscore

// [dev] diagnostics for hideck#90 (Vodafone DE refresh 403 after port-s RST).
// Every line goes through devLog with the "[dev]" prefix so the whole set can
// be found and removed or downgraded before a PR. SIP text goes through
// devRedactSIP: Authorization, the user part of every sip:/tel: URI, quoted
// display names, long digit runs, the IMEI in +sip.instance and Path, and the
// WLAN node id are masked. MESSAGE bodies are never logged.

import (
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

var (
	devIMEIPattern       = regexp.MustCompile(`(?i)(imei:)[0-9a-z*-]+`)
	devWLANNodeIDPattern = regexp.MustCompile(`(?i)(i-wlan-node-id=)"?[0-9a-f:.*-]+"?`)
	// The user part of sip:, sips: and tel: URIs carries IMPI, IMPU and MSISDN
	// in any format (separators, letters, ';' and ','); Contact's random user
	// goes too.
	devSIPUserPattern = regexp.MustCompile(`(?i)\b(sips?:)[^@\s<>"]+@`)
	devTelPattern     = regexp.MustCompile(`(?i)\b(tel:)[^\s<>";,]+`)
	// A quoted display name in front of an address, anywhere.
	devDisplayNamePattern = regexp.MustCompile(`"[^"\r\n]*"(\s*<)`)
	// An unquoted display name in an address header value.
	devTokenDisplayNamePattern = regexp.MustCompile(`(^|,)(\s*)([^<>",]*[^<>",\s])(\s*<)`)
	devReginfoDisplayName      = regexp.MustCompile(`(?i)(<display-name[^>]*>)[^<]*(</display-name>)`)
	// Headers whose values are addresses that may carry a display name.
	devAddressHeaders = map[string]bool{
		"from": true, "f": true, "to": true, "t": true, "contact": true, "m": true,
		"p-asserted-identity": true, "p-preferred-identity": true, "p-associated-uri": true,
		"p-called-party-id": true, "referred-by": true, "b": true, "refer-to": true, "r": true,
		"diversion": true, "history-info": true,
	}
	// RedactSIPRaw keeps the first and last digits of long runs.
	devPartialDigitsPattern = regexp.MustCompile(`\d*\*{3,}\d*`)
	devBodySeparator        = regexp.MustCompile(`\r?\n\r?\n`)
)

// devTraceState holds what the [dev] lines compare across REGISTERs.
type devTraceState struct {
	mu sync.Mutex
	// lastRequest is the previous REGISTER of the same Call-ID, unredacted.
	// It stays in memory; only the names of changed headers are logged.
	lastRequest       string
	lastRequestCallID string
	// pendingTrigger is the reason of the last triggerRegisterImmediate;
	// registerLocked moves it to trigger, which every attempt of that
	// registration (transport fallback, in-tunnel initial registration) logs.
	pendingTrigger string
	trigger        string
	// inboundCounts samples periodic inbound requests.
	inboundCounts map[sip.RequestMethod]uint64
}

func devSampled(count uint64) bool {
	for count >= 10 && count%10 == 0 {
		count /= 10
	}
	return count == 1
}

func devLog(msg string, args ...any) {
	logging.Info("[dev] "+msg, args...)
}

// devRedactSIP works per logical line: headers are unfolded first so a
// continuation of Authorization goes with it, and header names are compared
// after trimming, since RFC 3261 allows whitespace before the colon.
func devRedactSIP(raw string) string {
	lines := strings.Split(string(unfoldSIPHeaders([]byte(raw))), "\n")
	for index, line := range lines {
		name, _, isHeader := strings.Cut(line, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		switch {
		case isHeader && (name == "authorization" || name == "proxy-authorization"):
			lines[index] = strings.TrimSpace(strings.SplitN(line, ":", 2)[0]) + ": [REDACTED]"
			if strings.HasSuffix(line, "\r") {
				lines[index] += "\r"
			}
		case isHeader && strings.HasPrefix(name, "security-"):
			// SPIs, ports and algorithms carry no identity; kept so SA
			// changes stay visible.
		default:
			lines[index] = devRedactSIPLine(line, isHeader && devAddressHeaders[name])
		}
	}
	return strings.Join(lines, "\n")
}

func devRedactSIPLine(line string, address bool) string {
	redacted := logging.RedactSIPRaw(line)
	redacted = devIMEIPattern.ReplaceAllString(redacted, "${1}<masked>")
	redacted = devWLANNodeIDPattern.ReplaceAllString(redacted, `${1}"<masked>"`)
	redacted = devSIPUserPattern.ReplaceAllString(redacted, "${1}<masked>@")
	redacted = devTelPattern.ReplaceAllString(redacted, "${1}<masked>")
	redacted = devDisplayNamePattern.ReplaceAllString(redacted, `"<masked>"${1}`)
	redacted = devReginfoDisplayName.ReplaceAllString(redacted, "${1}<masked>${2}")
	if address {
		name, value, _ := strings.Cut(redacted, ":")
		redacted = name + ":" + devTokenDisplayNamePattern.ReplaceAllString(value, "${1}${2}<masked>${4}")
	}
	return devPartialDigitsPattern.ReplaceAllString(redacted, "<masked>")
}

func (s *Service) devRecordTrigger(reason string) {
	s.dev.mu.Lock()
	s.dev.pendingTrigger = strings.TrimSpace(reason)
	s.dev.mu.Unlock()
}

// devBeginRegister starts the trigger of one registerLocked run.
func (s *Service) devBeginRegister() {
	s.dev.mu.Lock()
	s.dev.trigger = s.dev.pendingTrigger
	s.dev.pendingTrigger = ""
	if s.dev.trigger == "" {
		s.dev.trigger = "scheduled"
	}
	s.dev.mu.Unlock()
}

func (s *Service) devSetTrigger(reason string) {
	s.dev.mu.Lock()
	s.dev.trigger = reason
	s.dev.mu.Unlock()
}

// devLogRegisterContext records why and when a REGISTER attempt starts.
func (s *Service) devLogRegisterContext(session *registerSession) {
	if s == nil || session == nil {
		return
	}
	s.dev.mu.Lock()
	trigger := s.dev.trigger
	s.dev.mu.Unlock()
	s.mu.RLock()
	lastOK := s.lastRegisterOKAt
	s.mu.RUnlock()
	portS := s.capturePortSSession()
	devLog("IMS REGISTER attempt context",
		"device", s.DeviceID(), "kind", map[bool]string{true: "refresh", false: "initial"}[session.refresh],
		"cseq", session.cseq, "trigger", trigger,
		"since_register_ok", devSince(lastOK),
		"port_s_connected", portS.connected, "port_s_generation", portS.generation,
		"port_s_close_kind", portS.lastCloseKind, "since_port_s_close", devSince(portS.closedAt),
		"carrier_preset", s.devCarrierPreset())
}

// devLogRegisterRequest logs the REGISTER and, within one Call-ID, the
// headers that differ from the previous REGISTER.
func (s *Service) devLogRegisterRequest(session *registerSession, request string) {
	if s == nil || session == nil {
		return
	}
	// Compare the unredacted requests so masked values (Authorization, URI
	// users) still show up as changed; only header names are logged.
	unfolded := string(unfoldSIPHeaders([]byte(request)))
	s.dev.mu.Lock()
	previous, previousCallID := s.dev.lastRequest, s.dev.lastRequestCallID
	s.dev.lastRequest, s.dev.lastRequestCallID = unfolded, session.callID
	s.dev.mu.Unlock()
	fields := []any{"device", s.DeviceID(), "cseq", session.cseq, "sip", devRedactSIP(request)}
	if previous != "" && previousCallID == session.callID {
		fields = append(fields, "changed_headers", devChangedSIPHeaders(previous, unfolded))
	}
	devLog("IMS REGISTER outbound", fields...)
}

func (s *Service) devLogRegisterResponse(session *registerSession, response *sipResponse, elapsed time.Duration) {
	if s == nil || session == nil || response == nil {
		return
	}
	raw := ""
	if response.parsed != nil {
		raw = devRedactSIP(response.parsed.String())
	}
	devLog("IMS REGISTER inbound", "device", s.DeviceID(), "cseq", session.cseq,
		"status", response.StatusCode, "reason", response.Reason,
		"elapsed_ms", elapsed.Milliseconds(), "sip", raw)
}

// devLogRefreshRejected summarizes a refresh that got a final failure.
func (s *Service) devLogRefreshRejected(err error, next string) {
	if s == nil {
		return
	}
	s.mu.RLock()
	lastOK := s.lastRegisterOKAt
	s.mu.RUnlock()
	portS := s.capturePortSSession()
	devLog("IMS refresh rejected",
		"device", s.DeviceID(), "err", err, "next", next,
		"since_register_ok", devSince(lastOK),
		"port_s_connected", portS.connected, "port_s_close_kind", portS.lastCloseKind,
		"since_port_s_close", devSince(portS.closedAt), "port_s_peer_resets", portS.peerResetCount,
		"carrier_preset", s.devCarrierPreset())
}

func (s *Service) devLogPortSEvent(event string, generation uint64, conn net.Conn) {
	if s == nil {
		return
	}
	s.mu.RLock()
	lastOK := s.lastRegisterOKAt
	s.mu.RUnlock()
	portS := s.capturePortSSession()
	fields := []any{"device", s.DeviceID(), "event", event, "generation", generation,
		"since_register_ok", devSince(lastOK), "since_last_inbound", devSince(portS.lastInboundAt),
		"since_opened", devSince(portS.openedAt), "carrier_preset", s.devCarrierPreset()}
	if conn != nil {
		fields = append(fields, "remote", conn.RemoteAddr(), "local", conn.LocalAddr())
	}
	devLog("IMS port-s timing", fields...)
}

// devLogInboundRequest logs every request the P-CSCF sends us. MESSAGE keeps
// its headers only, so SMS content never reaches the log.
func (s *Service) devLogInboundRequest(request *sip.Request, raw string, peer net.Conn) {
	if s == nil || request == nil {
		return
	}
	s.dev.mu.Lock()
	if s.dev.inboundCounts == nil {
		s.dev.inboundCounts = make(map[sip.RequestMethod]uint64)
	}
	s.dev.inboundCounts[request.Method]++
	count := s.dev.inboundCounts[request.Method]
	s.dev.mu.Unlock()
	// OPTIONS can be periodic; keep its 1st, 10th, 100th... occurrence.
	if request.Method == sip.OPTIONS && !devSampled(count) {
		return
	}
	if request.Method == sip.MESSAGE {
		if end := devBodySeparator.FindStringIndex(raw); end != nil {
			raw = raw[:end[0]]
		}
	}
	s.mu.RLock()
	lastOK := s.lastRegisterOKAt
	s.mu.RUnlock()
	portS := s.capturePortSSession()
	fields := []any{"device", s.DeviceID(), "method", string(request.Method), "count", count,
		"since_register_ok", devSince(lastOK), "port_s_generation", portS.generation,
		"since_port_s_opened", devSince(portS.openedAt), "sip", devRedactSIP(raw)}
	if peer != nil {
		fields = append(fields, "remote", peer.RemoteAddr(), "local", peer.LocalAddr())
	}
	devLog("IMS inbound request", fields...)
}

func (s *Service) devCarrierPreset() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return strings.TrimSpace(s.cfg.CarrierPresetID)
}

func devSince(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return time.Since(at).Round(time.Millisecond).String()
}

// devChangedSIPHeaders lists header names whose values differ between two
// requests, ignoring the ones that change on every request.
func devChangedSIPHeaders(previous, current string) []string {
	before, after := devSIPHeaders(previous), devSIPHeaders(current)
	changed := make([]string, 0)
	for name, value := range after {
		if before[name] != value {
			changed = append(changed, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			changed = append(changed, name+"(removed)")
		}
	}
	sort.Strings(changed)
	return changed
}

func devSIPHeaders(raw string) map[string]string {
	headers := make(map[string]string)
	head, _, _ := strings.Cut(raw, "\r\n\r\n")
	lines := strings.Split(strings.ReplaceAll(head, "\r\n", "\n"), "\n")
	for _, line := range lines[min(1, len(lines)):] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		switch name {
		case "cseq", "via", "v", "content-length", "l":
			continue
		}
		if headers[name] != "" {
			headers[name] += "\n"
		}
		headers[name] += strings.TrimSpace(value)
	}
	return headers
}

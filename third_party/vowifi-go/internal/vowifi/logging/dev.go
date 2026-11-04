package logging

// [dev] helpers for the debug-field branch. Every line goes through DevLog
// with the "[dev]" prefix so the whole set can be found by that prefix.
// DevRedactSIP masks Authorization, digest nonces, the user part of every
// sip:/tel: URI, display names, long digit runs, the IMEI in +sip.instance and
// Path, and the WLAN node id. Bodies are kept only for text types that carry
// no message content; an SMS body never reaches the log.

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	devIMEIPattern       = regexp.MustCompile(`(?i)(imei:)[0-9a-z*-]+`)
	devWLANNodeIDPattern = regexp.MustCompile(`(?i)(i-wlan-node-id=)"?[0-9a-f:.*-]+"?`)
	// The user part of sip:, sips: and tel: URIs carries IMPI, IMPU and MSISDN
	// in any format (separators, letters, ';' and ','); Contact's random user
	// goes too.
	devSIPUserPattern = regexp.MustCompile(`(?i)\b(sips?:)[^@\s<>"]+@`)
	devTelPattern     = regexp.MustCompile(`(?i)\b(tel:)[^\s<>";,]+`)
	// A quoted display name in front of an address: at the start of a header
	// value or of a list item, not a quoted parameter such as +sip.instance.
	devDisplayNamePattern = regexp.MustCompile(`(^|[:,]\s*)"[^"\r\n]*"(\s*<)`)
	// An unquoted display name in an address header value.
	devTokenDisplayNamePattern = regexp.MustCompile(`(^|,)(\s*)([^<>",]*[^<>",\s])(\s*<)`)
	devReginfoDisplayName      = regexp.MustCompile(`(?i)(<display-name[^>]*>)[^<]*(</display-name>)`)
	// nonce carries RAND and AUTN; username carries the IMPI.
	devDigestParamPattern = regexp.MustCompile(`(?i)\b(nonce|nextnonce|cnonce|response|rspauth|username|auts|ck|ik)\s*=\s*("[^"]*"|[^,\s]+)`)
	// SDP keying material (RFC 4568 a=crypto inline keys, k=, key-mgmt).
	devSDPInlineKey = regexp.MustCompile(`(?i)(inline:)[^\s|;]+`)
	// Headers whose values are addresses that may carry a display name.
	devAddressHeaders = map[string]bool{
		"from": true, "f": true, "to": true, "t": true, "contact": true, "m": true,
		"p-asserted-identity": true, "p-preferred-identity": true, "p-associated-uri": true,
		"p-called-party-id": true, "referred-by": true, "b": true, "refer-to": true, "r": true,
		"diversion": true, "history-info": true,
	}
	// RedactSIPRaw keeps the first and last digits of long runs.
	devPartialDigitsPattern = regexp.MustCompile(`\d*\*{3,}\d*`)
	// Body types worth reading that carry no SMS or call content.
	devTextBodyTypes = []string{"application/sdp", "application/reginfo+xml",
		"application/simple-message-summary", "application/3gpp-ims+xml", "application/pidf+xml"}
)

// DevLog writes one [dev] line at Info so it shows without server.debug.
func DevLog(msg string, args ...any) {
	Info("[dev] "+msg, args...)
}

// DevSampled is true for the 1st, 10th, 100th, ... occurrence.
func DevSampled(count uint64) bool {
	for count >= 10 && count%10 == 0 {
		count /= 10
	}
	return count == 1
}

// DevRedactSIP returns a SIP message safe to log: headers unfolded and
// redacted line by line; the body kept and redacted for the text types in
// devTextBodyTypes, otherwise replaced by its type and length.
func DevRedactSIP(raw string) string {
	head, body, hasBody := devCutSIPBody(raw)
	lines := strings.Split(devUnfoldSIPHeaders(head), "\n")
	contentType := ""
	for index, line := range lines {
		name, value, isHeader := strings.Cut(line, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		if isHeader && (name == "content-type" || name == "c") {
			contentType = strings.ToLower(strings.TrimSpace(value))
		}
		switch {
		case isHeader && (name == "authorization" || name == "proxy-authorization" ||
			name == "authentication-info" || name == "proxy-authentication-info"):
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
	out := strings.Join(lines, "\n")
	if !hasBody {
		return out
	}
	if body == "" {
		return out + devBodySeparator(head)
	}
	for _, kind := range devTextBodyTypes {
		if strings.HasPrefix(contentType, kind) {
			bodyLines := strings.Split(body, "\n")
			for index, line := range bodyLines {
				bodyLines[index] = devRedactBodyLine(line)
			}
			return out + devBodySeparator(head) + strings.Join(bodyLines, "\n")
		}
	}
	return out + devBodySeparator(head) + fmt.Sprintf("<body omitted: %q, %d bytes>", contentType, len(body))
}

// devRedactBodyLine masks identities and any SDP keying material.
func devRedactBodyLine(line string) string {
	trimmed := strings.ToLower(strings.TrimSpace(line))
	switch {
	case strings.HasPrefix(trimmed, "k="):
		return "k=<masked>"
	case strings.HasPrefix(trimmed, "a=key-mgmt:"):
		return "a=key-mgmt:<masked>"
	}
	return devRedactSIPLine(devSDPInlineKey.ReplaceAllString(line, "${1}<masked>"), false)
}

// DevRedactText masks identities in free text such as a JSON status dump.
func DevRedactText(text string) string {
	return devRedactSIPLine(text, false)
}

func devRedactSIPLine(line string, address bool) string {
	redacted := RedactSIPRaw(line)
	redacted = devDigestParamPattern.ReplaceAllString(redacted, `${1}="<masked>"`)
	redacted = devIMEIPattern.ReplaceAllString(redacted, "${1}<masked>")
	redacted = devWLANNodeIDPattern.ReplaceAllString(redacted, `${1}"<masked>"`)
	redacted = devSIPUserPattern.ReplaceAllString(redacted, "${1}<masked>@")
	redacted = devTelPattern.ReplaceAllString(redacted, "${1}<masked>")
	redacted = devDisplayNamePattern.ReplaceAllString(redacted, `${1}"<masked>"${2}`)
	redacted = devReginfoDisplayName.ReplaceAllString(redacted, "${1}<masked>${2}")
	if address {
		name, value, _ := strings.Cut(redacted, ":")
		redacted = name + ":" + devTokenDisplayNamePattern.ReplaceAllString(value, "${1}${2}<masked>${4}")
	}
	return devPartialDigitsPattern.ReplaceAllString(redacted, "<masked>")
}

// devCutSIPBody splits at the first blank line, CRLF or bare LF.
func devCutSIPBody(raw string) (head, body string, hasBody bool) {
	crlf := strings.Index(raw, "\r\n\r\n")
	lf := strings.Index(raw, "\n\n")
	switch {
	case crlf >= 0 && (lf < 0 || crlf <= lf):
		return raw[:crlf], raw[crlf+4:], true
	case lf >= 0:
		return raw[:lf], raw[lf+2:], true
	}
	return raw, "", false
}

func devBodySeparator(head string) string {
	if strings.Contains(head, "\r\n") {
		return "\r\n\r\n"
	}
	return "\n\n"
}

// devUnfoldSIPHeaders joins continuation lines (RFC 3261 §7.3.1) so a folded
// Authorization goes with its header.
func devUnfoldSIPHeaders(head string) string {
	lines := strings.Split(head, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(out) > 0 && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			previous := strings.TrimSuffix(out[len(out)-1], "\r")
			out[len(out)-1] = previous + " " + strings.TrimLeft(line, " \t")
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

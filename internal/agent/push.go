package agent

import (
	"strconv"
	"time"
	"unicode/utf8"
)

// pushParams appends the params of one metrics.push to dst[:0] in a single
// pass: the Prometheus text is escaped straight into the buffer. The generic
// path (json.Marshal of a map holding string(text), then the RawMessage
// compaction rpc.Notification does) copied and scanned the 20-40 KB text three
// times, which on a MIPS access point cost more than collecting it. The
// result goes out through link.Session.NotifyRaw.
func pushParams(dst, text []byte, collectedAt time.Time, took time.Duration, seq uint64) []byte {
	b := append(dst[:0], `{"format":"prometheus-text","collectedAt":"`...)
	b = collectedAt.UTC().AppendFormat(b, time.RFC3339)
	b = append(b, `","durationMs":`...)
	b = strconv.AppendInt(b, took.Milliseconds(), 10)
	b = append(b, `,"seq":`...)
	b = strconv.AppendUint(b, seq, 10)
	b = append(b, `,"text":`...)
	b = appendJSONString(b, text)
	return append(b, '}')
}

const hexDigits = "0123456789abcdef"

// appendJSONString appends s as a JSON string. Like encoding/json it replaces
// each byte of invalid UTF-8 with U+FFFD (the controller's WebSocket library
// closes the session on a text frame that is not valid UTF-8, and an SSID can
// be any bytes) and escapes U+2028 and U+2029; unlike it, it leaves <, > and &
// alone, since nothing here is embedded in HTML.
func appendJSONString(dst, s []byte) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRune(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			dst = append(dst, s[start:i]...)
			dst = append(dst, `�`...)
		case r == ' ' || r == ' ':
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0xf])
		default:
			i += size
			continue
		}
		i += size
		start = i
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

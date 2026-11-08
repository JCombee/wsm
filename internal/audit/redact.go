package audit

import (
	"regexp"
	"strings"
)

// Redacted replaces every secret value before an event is stored.
const Redacted = "[REDACTED]"

// sensitiveKeys are matched against object keys after lowercasing and
// removing "-" and "_". A key matches when it contains one of them.
var sensitiveKeys = []string{
	"password", "passwd", "passphrase", "secret", "token", "apikey",
	"authorization", "credential", "privatekey", "cookie", "session",
}

func isSensitiveKey(k string) bool {
	k = strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(k))
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// Secrets that show up inside free text.
var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Authorization header values.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`), "$1 " + Redacted},
	// user:password@ in URLs and DSNs.
	{regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]*):[^@\s/]*@`), "$1:" + Redacted + "@"},
	// key=value and key: value pairs with a sensitive key.
	{regexp.MustCompile(`(?i)\b([a-z0-9_-]*(?:password|passwd|secret|token|api[_-]?key)[a-z0-9_-]*)\s*([=:])\s*("[^"]*"|'[^']*'|[^\s,;&]+)`), "$1$2" + Redacted},
}

func redactString(s string) string {
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// redactValue returns v with secret values replaced. Values under a
// sensitive key are replaced whole; other strings are scanned for secrets.
func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSensitiveKey(k) {
				out[k] = Redacted
			} else {
				out[k] = redactValue(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactValue(val)
		}
		return out
	case string:
		return redactString(t)
	default:
		return v
	}
}

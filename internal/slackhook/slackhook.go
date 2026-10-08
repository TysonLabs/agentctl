// Package slackhook validates a Slack incoming-webhook URL and scrubs text
// that may contain one. agentflow checks a webhook before it posts and
// agentcfg before it stores one, so both accept exactly the same shape, and
// both clean displayed text the same way. It holds no secrets.
package slackhook

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// pathRe is Slack's shape: /services/<team>/<bot>/<secret>. Requiring 6+
// chars per segment also keeps agentflow's redaction from blanking short
// words in a message body.
var pathRe = regexp.MustCompile(`^/services/[A-Za-z0-9]{6,}/[A-Za-z0-9]{6,}/[A-Za-z0-9]{6,}$`)

var (
	secretRes = []*regexp.Regexp{
		regexp.MustCompile(`https?://hooks\.slack\.com/\S*`),
		regexp.MustCompile(`xox[a-z]-[A-Za-z0-9-]+`),
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`\b[0-9A-Fa-f]{48,}\b`), // hex tokens; a 40-char commit SHA stays
	}
	base64Re = regexp.MustCompile(`[A-Za-z0-9+/]{32,}={0,2}`)
)

// Valid reports whether u is an https://hooks.slack.com/services/... webhook
// with no userinfo, query or fragment.
func Valid(u string) bool {
	p, err := url.Parse(u)
	return u != "" && err == nil && p.Scheme == "https" && p.Host == "hooks.slack.com" &&
		pathRe.MatchString(p.Path) && p.User == nil && p.RawQuery == "" && p.Fragment == ""
}

// CleanText removes the webhook (including its identifying path segments),
// other credential-shaped text, and characters that can hide or reorder
// Slack output. It is shared by agentflow's payload builder and agentcfg's
// announcement views so the settings surfaces cannot reflect a webhook.
func CleanText(s, webhook string) string {
	s = RedactSecret(s, webhook)
	for _, re := range secretRes {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	s = base64Re.ReplaceAllStringFunc(s, func(m string) string {
		if looksLikeBase64Secret(m) {
			return "[redacted]"
		}
		return m
	})
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}

// SafeLabel reports whether agentflow would keep a channel label unchanged.
func SafeLabel(label, webhook string) bool { return CleanText(label, webhook) == label }

// RedactSecret removes a complete webhook and each identifying path segment.
func RedactSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	s = strings.ReplaceAll(s, secret, "[redacted]")
	if u, err := url.Parse(secret); err == nil && len(u.Path) > len("/services/") {
		tail := strings.TrimPrefix(u.Path, "/services/")
		s = strings.ReplaceAll(s, tail, "[redacted]")
		// Any path component can identify the incoming webhook. Errors from
		// proxies and transports sometimes print components separately.
		for _, part := range strings.Split(tail, "/") {
			if part != "" {
				s = strings.ReplaceAll(s, part, "[redacted]")
			}
		}
	}
	return s
}

// looksLikeBase64Secret flags base64 runs with mixed case, a digit and a
// '+' or '=': random keys have them, file paths and prose almost never do.
func looksLikeBase64Secret(s string) bool {
	var upper, lower, digit bool
	for _, r := range s {
		upper = upper || unicode.IsUpper(r)
		lower = lower || unicode.IsLower(r)
		digit = digit || unicode.IsDigit(r)
	}
	return upper && lower && digit && strings.ContainsAny(s, "+=")
}

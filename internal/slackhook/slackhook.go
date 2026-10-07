// Package slackhook validates a Slack incoming-webhook URL. agentflow checks
// a webhook before it posts and agentcfg checks one before it stores it, so
// both accept exactly the same shape. It holds no secrets.
package slackhook

import (
	"net/url"
	"regexp"
)

// pathRe is Slack's shape: /services/<team>/<bot>/<secret>. Requiring 6+
// chars per segment also keeps agentflow's redaction from blanking short
// words in a message body.
var pathRe = regexp.MustCompile(`^/services/[A-Za-z0-9]{6,}/[A-Za-z0-9]{6,}/[A-Za-z0-9]{6,}$`)

// Valid reports whether u is an https://hooks.slack.com/services/... webhook
// with no userinfo, query or fragment.
func Valid(u string) bool {
	p, err := url.Parse(u)
	return u != "" && err == nil && p.Scheme == "https" && p.Host == "hooks.slack.com" &&
		pathRe.MatchString(p.Path) && p.User == nil && p.RawQuery == "" && p.Fragment == ""
}

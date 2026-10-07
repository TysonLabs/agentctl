package slackhook

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	good := "https://hooks.slack.com/services/T0ABCDEF/B0ABCDEF/abcdefXYZ123"
	if !Valid(good) {
		t.Fatalf("Valid(%q) = false", good)
	}
	for _, bad := range []string{
		"",
		"http://hooks.slack.com/services/T0ABCDEF/B0ABCDEF/abcdefXYZ123",
		"https://hooks.slack.com.evil.com/services/T0ABCDEF/B0ABCDEF/abcdefXYZ123",
		"https://hooks.slack.com/services/T0/B0ABCDEF/abcdefXYZ123",
		"https://hooks.slack.com/services/T0ABCDEF/B0ABCDEF/abcdefXYZ123?x=1",
		"https://hooks.slack.com/services/T0ABCDEF/B0ABCDEF/abcdefXYZ123#f",
		"https://u:p@hooks.slack.com/services/T0ABCDEF/B0ABCDEF/abcdefXYZ123",
		"https://hooks.slack.com/services/T0ABCDEF/B0ABCDEF/abc/extra",
	} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true", bad)
		}
	}
}

func TestCleanTextAndSafeLabel(t *testing.T) {
	const hook = "https://hooks.slack.com/services/T0ABCDEF/B0ABCDEF/abcdefXYZ123"
	if !SafeLabel("#releases", hook) {
		t.Fatal("ordinary channel label is unsafe")
	}
	for _, label := range []string{hook, "abcdefXYZ123", "xoxb-1234-abcdefghijkl"} {
		if SafeLabel(label, hook) {
			t.Errorf("SafeLabel(%q) = true", label)
		}
		if got := CleanText(label, hook); strings.Contains(got, "abcdefXYZ123") || strings.Contains(got, "xoxb-") {
			t.Errorf("CleanText(%q) leaked credential material: %q", label, got)
		}
	}
}

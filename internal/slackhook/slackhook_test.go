package slackhook

import "testing"

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

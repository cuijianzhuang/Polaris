package sub

import (
	"strings"
	"testing"
)

// The portal's scripts read their runtime labels from the injected table, so a
// missing key renders as an empty string. Every key must exist in both
// languages and carry a value.
func TestPortalJSStringsCoverBothLanguages(t *testing.T) {
	for key, zh := range portalJSChinese {
		if zh == "" {
			t.Errorf("%q has an empty Chinese value", key)
		}
		en, ok := portalJSEnglish[key]
		if !ok {
			t.Errorf("%q is missing from the English table", key)
			continue
		}
		if en == "" {
			t.Errorf("%q has an empty English value", key)
		}
		// Placeholders must match, or s() leaves a literal {name} on screen.
		for _, ph := range placeholders(zh) {
			if !strings.Contains(en, "{"+ph+"}") {
				t.Errorf("%q: English value lost the %q placeholder", key, ph)
			}
		}
	}
	for key := range portalJSEnglish {
		if _, ok := portalJSChinese[key]; !ok {
			t.Errorf("%q is missing from the Chinese table", key)
		}
	}
}

func placeholders(s string) []string {
	var out []string
	for {
		open := strings.IndexByte(s, '{')
		if open < 0 {
			return out
		}
		close := strings.IndexByte(s[open:], '}')
		if close < 0 {
			return out
		}
		out = append(out, s[open+1:open+close])
		s = s[open+close:]
	}
}

func TestPortalJSStringsPicksLanguage(t *testing.T) {
	en := portalJSStrings("en")
	if !strings.Contains(en, `"viewOverview":"Dashboard"`) {
		t.Fatalf("English table not selected: %s", en[:120])
	}
	zh := portalJSStrings("zh-CN")
	if !strings.Contains(zh, `"viewOverview":"仪表盘"`) {
		t.Fatalf("Chinese table not selected: %s", zh[:120])
	}
	// An unknown locale falls back to Chinese rather than emitting nothing.
	if got := portalJSStrings("fr"); got != zh {
		t.Fatal("unknown locale must fall back to the Chinese table")
	}
	if !strings.HasPrefix(zh, "window.__portalI18n={") || !strings.HasSuffix(zh, "};") {
		t.Fatal("emitted table is not a JS assignment")
	}
}

// The generated literal must be stable so the page output stays comparable.
func TestPortalJSStringsIsDeterministic(t *testing.T) {
	if portalJSStrings("zh-CN") != portalJSStrings("zh-CN") {
		t.Fatal("output changed between calls")
	}
}

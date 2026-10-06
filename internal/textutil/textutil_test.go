package textutil

import (
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"plain text":                 "plain text",
		"two\nlines":                 "two\nlines",
		"tab\there":                  "tab    here",
		"\x1b[31mred\x1b[0m":         "red",
		"\x1b[2J\x1b[Hcleared":       "cleared",
		"title\x1b]0;evil\x07 after": "title after",
		"link\x1b]8;;http://x\x1b\\t\x1b]8;;\x1b\\": "linkt",
		"clip\x1b]52;c;ZWNobyBwd25lZA==\x07":        "clip",
		"cr\rover":                                  "crover",
		"bell\x07 nul\x00 del\x7f":                  "bell nul del",
		"c1 \u009b31m csi":                          "c1 31m csi",
		"rtl \u202eevil\u202c":                      "rtl evil",
		"unicode ✓ → ok":                            "unicode ✓ → ok",
		"bad \xff utf8":                             "bad \ufffd utf8",
		"dangling \x1b":                             "dangling ",
	}
	for in, want := range cases {
		got := Sanitize(in)
		if got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, "\x1b\x07\x00\r") {
			t.Errorf("Sanitize(%q) left a control character: %q", in, got)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 10); got != "short" {
		t.Errorf("%q", got)
	}
	got := Truncate("héllo wörld, this is long", 8)
	if !strings.HasPrefix(got, "héllo ") || !strings.Contains(got, "more bytes") {
		t.Errorf("%q", got)
	}
	tail := TruncateHead("line1\nline2\nline3\nthe end", 12)
	if !strings.HasSuffix(tail, "the end") || !strings.Contains(tail, "omitted") || strings.Contains(tail, "line1") {
		t.Errorf("%q", tail)
	}
	if OneLine("a\n  b\t c ") != "a b c" {
		t.Error("OneLine")
	}
}

package textx

import (
	"testing"
	"unicode/utf8"
)

func TestStripANSI(t *testing.T) {
	cases := map[string]string{
		"\x1b[1;32mgreen\x1b[0m":                   "green",
		"a\r\nb\r\n":                               "a\nb\n",
		"\x1b]0;title\x07prompt$ ":                 "prompt$ ",
		"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\": "link",
		"\x1b(Bplain\x1b=":                         "plain",
		"tab\there\x07":                            "tab\there",
		"progress\rdone":                           "progressdone",
		"\x1b[?2004hbash$ \x1b[?2004l":             "bash$ ",
		"cut\x1b":                                  "cut",
	}
	for in, want := range cases {
		if got := string(StripANSI([]byte(in))); got != want {
			t.Errorf("StripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCutAndTailUTF8(t *testing.T) {
	s := []byte("aé€😀b") // 1+2+3+4+1 bytes
	for n := 0; n <= len(s); n++ {
		c := CutUTF8(s, n)
		if len(c) > n || !utf8.Valid(c) {
			t.Errorf("CutUTF8(%d) = %q", n, c)
		}
		tl := TailUTF8(s, n)
		if len(tl) > n || !utf8.Valid(tl) {
			t.Errorf("TailUTF8(%d) = %q", n, tl)
		}
	}
	if string(CutUTF8(s, 4)) != "aé" {
		t.Errorf("CutUTF8(4) = %q", CutUTF8(s, 4))
	}
}

func TestLooksBinary(t *testing.T) {
	if LooksBinary([]byte("hello\nworld")) {
		t.Error("text flagged as binary")
	}
	if !LooksBinary([]byte("ELF\x00\x01")) {
		t.Error("NUL not flagged")
	}
	bad := make([]byte, 100)
	for i := range bad {
		bad[i] = 0xff
	}
	if !LooksBinary(bad) {
		t.Error("invalid UTF-8 not flagged")
	}
}

package cli

import (
	"bytes"
	"testing"
)

// Recorded output is untrusted: an agent prints whatever a file or web page
// contained. --text must pass text and colour and nothing that acts on the
// viewer's terminal, even when a sequence straddles two events.
func TestTextFilterKeepsTextAndColourOnly(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "hello\r\n\tworld", "hello\r\n\tworld"},
		{"utf8", "héllo → 世界\n", "héllo → 世界\n"},
		{"sgr kept", "\x1b[1;31mred\x1b[0m", "\x1b[1;31mred\x1b[0m"},
		{"sgr colon kept", "\x1b[38:2:1:2:3mx", "\x1b[38:2:1:2:3mx"},
		{"osc52 clipboard dropped", "a\x1b]52;c;aGVsbG8=\x07b", "ab"},
		{"osc title dropped (ST)", "a\x1b]0;evil title\x1b\\b", "ab"},
		{"decrqss query dropped", "a\x1bP$qm\x1b\\b", "ab"},
		{"da/dsr queries dropped", "a\x1b[c\x1b[6n\x1b[>c\x1b[?6nb", "ab"},
		{"alt screen + paste dropped", "\x1b[?1049h\x1b[?2004hx\x1b[?2004l\x1b[?1049l", "x"},
		{"cursor and clear dropped", "\x1b[2J\x1b[H\x1b[3Jx\x1b[10;20H", "x"},
		{"private sgr-looking dropped", "\x1b[?1mx", "x"},
		{"c0 controls dropped", "a\x07\x08\x0c\x0e\x0f\x7fb", "ab"},
		{"two-byte esc dropped", "a\x1bcb\x1b7c\x1b(Bd", "abcd"},
		{"esc intermediate dropped", "a\x1b#8b", "ab"},
		{"apc/pm/sos dropped", "a\x1b_payload\x1b\\b\x1b^p\x07c\x1bXs\x1b\\d", "abcd"},
		{"long csi never sgr", "a\x1b[" + string(bytes.Repeat([]byte("1;"), 60)) + "mb", "ab"},
		{"c1 csi/osc/st replaced", "a\x9b6nb\x9d52;c;c2VjcmV0\x9cc", "a\uFFFD6nb\uFFFD52;c;c2VjcmV0\uFFFDc"},
		{"lone continuation replaced", "a\x80b\xbfc", "a\uFFFDb\uFFFDc"},
		{"overlong and invalid leads replaced", "a\xc0\xafb\xf5c\xffd", "a\uFFFD\uFFFDb\uFFFDc\uFFFDd"},
		{"truncated sequence replaced", "a\xe4\xb8b", "a\uFFFDb"},
		{"bel after esc ends osc", "a\x1b]0;title\x1b\x07b", "ab"},
		{"osc payload with c1 cannot escape", "a\x1b]52;c;\x9c\x9bmx\x07b", "ab"},
		{"overlong hiding a c1 csi", "a\xe0\x80\x9b6nb", "a\uFFFD6nb"},
		{"overlong 2-byte c1", "a\xc0\x9bb", "a\uFFFD\uFFFDb"},
		{"surrogate replaced", "a\xed\xa0\x80b", "a\uFFFDb"},
		{"beyond u+10ffff replaced", "a\xf4\x90\x80\x80b", "a\uFFFDb"},
		{"overlong osc and st", "a\xe0\x80\x9d52;c;x\xe0\x80\x9cb", "a\uFFFD52;c;x\uFFFDb"},
		{"literal replacement char kept", "a\xef\xbf\xbdb", "a\uFFFDb"},
		{"four-byte emoji kept", "a\xf0\x9f\x98\x80b", "a😀b"},
	}
	for _, c := range cases {
		var f textFilter
		if got := string(f.Write([]byte(c.in))); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestTextFilterSpansChunks(t *testing.T) {
	var f textFilter
	var out []byte
	for _, chunk := range []string{"a\x1b]52;c;", "aGVsbG8=", "\x07b\x1b[1", ";3", "1mc\x1b[", "0m\x1b[?104", "9hd\xe4", "\xb8", "\x96e\x9b", "6nf"} {
		out = append(out, f.Write([]byte(chunk))...)
	}
	if string(out) != "ab\x1b[1;31mc\x1b[0md世e\uFFFD6nf" {
		t.Fatalf("got %q", out)
	}
	// A whole-stream feed must agree with the chunked one.
	var g textFilter
	if whole := string(g.Write([]byte("a\x1b]52;c;aGVsbG8=\x07b\x1b[1;31mc\x1b[0m\x1b[?1049hd\xe4\xb8\x96e\x9b6nf"))); whole != string(out) {
		t.Fatalf("chunked %q vs whole %q", out, whole)
	}
}

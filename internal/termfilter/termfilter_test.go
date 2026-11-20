package termfilter

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func run(in string) string {
	var f Filter
	return string(f.Write([]byte(in)))
}

// context "what an agent can make a terminal DO is dropped, what it can make it DRAW passes": each case
// is one way out of the sandbox through the owner's terminal.
func TestDropsWhatActs(t *testing.T) {
	type tc struct{ in, want string } // want is what is left between an "A" and a "B" put around the input
	for name, c := range map[string]tc{
		"OSC 52 clipboard write, BEL":      {"\x1b]52;c;ZWNobyBwd25lZAo=\x07", ""},
		"OSC 52 clipboard write, ST":       {"\x1b]52;c;ZWNobyBwd25lZAo=\x1b\\", ""},
		"an OSC title (a lure)":            {"\x1b]0;sudo password:\x07", ""},
		"iTerm OSC 1337":                   {"\x1b]1337;File=inline=1:AAAA\x07", ""},
		"an OSC 8 hyperlink":               {"\x1b]8;;http://evil\x07", ""},
		"a DCS string":                     {"\x1bP$qm\x1b\\", ""},
		"an APC string":                    {"\x1b_Gf=100;AAAA\x1b\\", ""},
		"a PM string":                      {"\x1b^privacy\x1b\\", ""},
		"a SOS string":                     {"\x1bXsos\x1b\\", ""},
		"a device attributes query":        {"\x1b[c", ""},
		"a secondary DA query":             {"\x1b[>c", ""},
		"a cursor position report query":   {"\x1b[6n", ""},
		"a window operation (report size)": {"\x1b[18t", ""},
		"a title push":                     {"\x1b[22;0t", ""},
		"ESC Z (DECID)":                    {"\x1bZ", ""},
		"ENQ (answerback)":                 {"\x05", ""},
		"a raw C1 CSI (its introducer goes, the rest is plain text)": {"\x9b6n", "6n"},
		"a UTF-8 encoded C1 CSI":          {"\xc2\x9b6n", "6n"},
		"a UTF-8 encoded C1 OSC":          {"\xc2\x9d0;x", "0;x"},
		"SO and SI":                       {"\x0e\x0f", ""},
		"a status report request":         {"\x1b[?1;2c", ""},
		"a mouse mode":                    {"\x1b[?1000h", ""},
		"a kitty keyboard protocol query": {"\x1b[?u", ""},
		"a cursor style (DECSCUSR)":       {"\x1b[2 q", ""},
	} {
		if got := run("A" + c.in + "B"); got != "A"+c.want+"B" {
			t.Errorf("%s: %q passed as %q, want %q", name, c.in, got, "A"+c.want+"B")
		}
	}
}

func TestPassesWhatDraws(t *testing.T) {
	for name, in := range map[string]string{
		"plain text":                "hello, world 123",
		"UTF-8 text":                "žluťoučký kůň ✓ 日本語 ¡Â",
		"line controls":             "a\r\nb\tc\bd\x07",
		"SGR":                       "\x1b[0;1;38;5;208mx\x1b[m",
		"SGR with colons":           "\x1b[4:3mx",
		"cursor movement":           "\x1b[H\x1b[2;3H\x1b[5A\x1b[3B\x1b[2C\x1b[4D",
		"erase":                     "\x1b[2J\x1b[K\x1b[1K",
		"scroll margins and scroll": "\x1b[1;24r\x1b[2S\x1b[1T",
		"save and restore cursor":   "\x1b7\x1b8\x1b[s\x1b[u",
		"alternate screen":          "\x1b[?1049h\x1b[?1049l",
		"bracketed paste mode":      "\x1b[?2004h",
		"cursor visibility":         "\x1b[?25l\x1b[?25h",
		"index and reverse index":   "\x1bM\x1bD\x1bE",
		"keypad modes":              "\x1b=\x1b>",
	} {
		if got := run(in); got != in {
			t.Errorf("%s: %q became %q", name, in, got)
		}
	}
	// a character set designation is dropped whole (it cannot draw anything the text does not)
	if got := run("a\x1b(Bb"); got != "ab" {
		t.Errorf("charset designation: %q", got)
	}
}

// context "the stream arrives in arbitrary pieces": the same bytes in one write or one byte at a time
// give the same output.
func TestPiecesDoNotMatter(t *testing.T) {
	in := "x\x1b]52;c;AAAA\x07y\x1b[1;31mred\x1b[m\x1bP1$r\x1b\\z\xc2\x9bq\xc2\xa9 \x1b[6n!"
	whole := run(in)
	var f Filter
	var piece []byte
	for i := 0; i < len(in); i++ {
		piece = append(piece, f.Write([]byte{in[i]})...)
	}
	if string(piece) != whole || strings.ContainsAny(whole, "\x9b") || !strings.Contains(whole, "\xc2\xa9") {
		t.Errorf("whole %q pieces %q", whole, piece)
	}
}

// context "an unterminated string must not blank the screen for good or grow without limit": after the cap
// the filter goes back to text, and the text after it shows.
func TestAnUnterminatedStringIsAbandoned(t *testing.T) {
	var f Filter
	got := f.Write([]byte("\x1b]52;c;" + strings.Repeat("A", maxString+10)))
	got = append(got, f.Write([]byte("visible"))...)
	if !bytes.Contains(got, []byte("visible")) {
		t.Errorf("the screen stayed blank: %q", got)
	}
	// the payload of the abandoned string must not reach the screen as a command: it holds no escape
	if bytes.Contains(got, []byte{0x1b}) {
		t.Errorf("%q", got)
	}
}

// context "an escape inside a string that is not ST": it does not end the string, and a new sequence
// inside is not smuggled out.
func TestEscapeInsideAStringDoesNotEscapeIt(t *testing.T) {
	if got := run("\x1b]0;a\x1b[2Jb\x07tail"); got != "tail" {
		t.Errorf("%q", got)
	}
}

// the output never grows beyond twice the input
func TestOutputIsBounded(t *testing.T) {
	var f Filter
	in := bytes.Repeat([]byte("\xc2\x41\x1b[1m"), 1000)
	if got := f.Write(in); len(got) > 2*len(in) {
		t.Errorf("%d from %d", len(got), len(in))
	}
}

// context "a lead byte must not adopt a raw C1 byte as its continuation": the review's probes. A character
// is emitted only complete and valid, a cut-short one is dropped, and what interrupted it is looked at fresh.
func TestARawC1AfterAnInterruptedCharacterIsStillDropped(t *testing.T) {
	for name, c := range map[string]struct{ in, want string }{
		"a lead byte, a newline, then a raw OSC":         {"\xe2\n\x9d52;c;QUJD\x07", "\n52;c;QUJD\a"},
		"a lead byte, then ESC [ m, then a raw CSI":      {"\xc2\x1b[m\x9b6n", "\x1b[m6n"},
		"an overlong ESC":                                {"\xe0\x80\x9b", ""},
		"an overlong U+009D (OSC)":                       {"\xe0\x82\x9d", ""},
		"a surrogate":                                    {"\xed\xa0\x80", ""},
		"above U+10FFFF":                                 {"\xf4\x90\x80\x80", ""},
		"an overlong four-byte form":                     {"\xf0\x80\x80\x9b", ""},
		"C0 and C1 lead bytes":                           {"\xc0\x9b\xc1\x9d", ""},
		"a lead byte then a lead byte":                   {"\xe2\xe2\x82\xac", "\xe2\x82\xac"},
		"a lead byte, ASCII, then its continuations":     {"\xe2a\x82\xac", "a"},
		"a lead byte at the end of a line":               {"\xf0\x9f\r\n", "\r\n"},
		"U+0080 and U+009F encoded":                      {"\xc2\x80\xc2\x9f", ""},
		"U+00A0 encoded (not a control)":                 {"\xc2\xa0", "\xc2\xa0"},
		"a valid two, three and four byte character":     {"é€😀", "é€😀"},
		"the boundaries E0 A0 80, ED 9F BF, F4 8F BF BF": {"\xe0\xa0\x80\xed\x9f\xbf\xf4\x8f\xbf\xbf", "\xe0\xa0\x80\xed\x9f\xbf\xf4\x8f\xbf\xbf"},
	} {
		if got := run("A" + c.in + "B"); got != "A"+c.want+"B" {
			t.Errorf("%s: %q gave %q, want %q", name, c.in, got, "A"+c.want+"B")
		}
		// the same, one byte per Write
		var f Filter
		var piece []byte
		for _, b := range []byte("A" + c.in + "B") {
			piece = append(piece, f.Write([]byte{b})...)
		}
		if string(piece) != "A"+c.want+"B" {
			t.Errorf("%s, byte by byte: %q", name, piece)
		}
	}
}

// context "whatever the agent sends": the output is valid UTF-8 with no C1 control and no escape except
// the ones the filter itself writes (ESC followed by [ or one of 78DEM=>c).
func TestWhateverIsSentTheOutputIsCleanUTF8(t *testing.T) {
	// every escape in the output is exactly one the filter is meant to write: a single-character escape, a
	// CSI of digits, ; and : ending in a drawing final, or a private-mode switch of a passed mode
	allow := regexp.MustCompile(`^(\x1b[78DEM=>c]|\x1b\[[0-9;:]*[mABCDEFGHfdJKLMP@XSTrsul` + "`" + `ea]|\x1b\[\?(` + strings.ReplaceAll(strings.Join(Modes, "|"), "?", "") + `)[hl])`)
	rnd := uint64(88172645463325252)
	next := func() uint64 { rnd ^= rnd << 13; rnd ^= rnd >> 7; rnd ^= rnd << 17; return rnd }
	alphabet := []byte{0x1b, '[', ']', 'P', '_', '^', 'X', 0x07, 0x9b, 0x9d, 0x90, 0xc2, 0xc3, 0xe0, 0xe2, 0xed, 0xf0, 0xf4, 0x80, 0xbf, 0xa0, '5', '2', ';', 'c', 'n', 'm', '?', '\\', 0x9c, 'a', 't', 'h', 'l', '1', '0', '4', 'H', ' ', 'q'}
	for i := 0; i < 4000; i++ {
		in := make([]byte, 64)
		for j := range in {
			if next()%4 == 0 {
				in[j] = byte(next())
			} else {
				in[j] = alphabet[next()%uint64(len(alphabet))]
			}
		}
		var f Filter
		var out []byte
		for rest := in; len(rest) > 0; { // split at random points, not at a fixed one
			n := 1 + int(next()%uint64(len(rest)))
			out = append(out, f.Write(rest[:n])...)
			rest = rest[n:]
		}
		if !utf8.Valid(out) {
			t.Fatalf("input %q gave invalid UTF-8 %q", in, out)
		}
		for _, r := range string(out) {
			if r >= 0x80 && r <= 0x9f {
				t.Fatalf("input %q gave a C1 control %q", in, out)
			}
		}
		for k := 0; k < len(out); k++ {
			if out[k] == 0x1b && !allow.Match(out[k:]) {
				t.Fatalf("input %q gave an escape that is not on the allow-list: %q", in, out[k:])
			}
		}
	}
}

func TestFocusReportingIsNotPassedAndEveryPassedModeIsListedForTheReset(t *testing.T) {
	if got := run("\x1b[?1004h"); got != "" {
		t.Errorf("focus reporting passed: %q", got)
	}
	for _, m := range Modes {
		if got := run("\x1b[" + m + "h"); got == "" {
			t.Errorf("mode %s is listed in Modes but dropped", m)
		}
	}
}

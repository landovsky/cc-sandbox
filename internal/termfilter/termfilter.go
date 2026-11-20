// Package termfilter removes from a byte stream everything a terminal could be made to ACT on, and keeps
// what it can only DRAW. It exists because the stream a pod's tmux sends is written by the sandboxed
// agent: an agent that can make the owner's terminal write the clipboard (OSC 52), answer a query into the
// owner's shell, change its title into a lure, or run a terminal-specific feature (DCS, APC, iTerm's OSC
// 1337) is talking out of the sandbox. The filter runs in a process with no credentials (ccsbx-term), so
// a bug in it is not a way to anything but the owner's terminal, which it already writes to.
//
// What passes: printable text (UTF-8 as is), a few C0 controls (BS TAB LF VT FF CR BEL), and the
// escape sequences a full-screen program draws with: SGR (colours and attributes), cursor movement and
// positioning, erase and edit, scrolling, scroll margins, cursor save and restore, and the mode switches
// for the alternate screen, bracketed paste, cursor visibility and autowrap. What is dropped: every OSC
// (titles included), DCS, APC, PM and SOS strings, C1 controls (raw and UTF-8 encoded), the device and
// status reports that make a terminal answer (DA, DSR, window reports), every other CSI, and every
// other escape. An unterminated string is dropped up to a cap and then abandoned, so it can neither
// hold the filter's memory nor blank the screen for good.
package termfilter

// UTF-8 only: a terminal in an 8-bit mode reads a valid two-byte character such as C3 9D as two bytes, the
// second of them a C1 control. The filter is sound for UTF-8 terminals, which is every terminal the owner
// attaches from (ccsbx-root sets LANG=C.UTF-8 for the session); it does not claim more.

// maxString is how many bytes of an OSC/DCS/APC/PM/SOS payload are swallowed before the filter gives up
// waiting for its terminator and goes back to ordinary text.
const maxString = 4096

type state int

const (
	text   state = iota
	esc          // after ESC
	escInt       // after ESC and an intermediate byte (0x20-0x2F): one more byte to drop
	csi          // inside a CSI, collecting parameters
	str          // inside OSC/DCS/APC/PM/SOS: dropping to ST or BEL
	strEsc       // inside a string, after ESC (maybe the start of ST)
)

// Filter is a streaming filter: Write can be given the stream in any pieces.
type Filter struct {
	st  state
	buf []byte // the CSI being read, so it can be passed or dropped whole
	n   int    // bytes swallowed in the current string
	osc bool   // the current string is an OSC (BEL ends it as well as ST)
	out []byte
	// a multi-byte UTF-8 character being read: it is emitted only once complete and valid, so a lead byte
	// can never adopt a following raw C1 byte as its "continuation"
	pend [4]byte
	pn   int // bytes held in pend
	need int // continuation bytes still wanted (0: not inside a character)
	lead byte
}

// csiFinal are the final bytes of the CSI sequences that only draw. Anything else is dropped.
// m SGR; A B C D E F G H f d cursor; J K erase; L M P @ X edit; S T scroll; r margins; s u cursor
// save and restore; h l mode set/reset (private modes are checked below); ` e a position.
const csiFinal = "mABCDEFGHfdJKLMP@XSTrsul`ea"

// allowedMode: the private modes whose switching only changes how the screen is drawn or what the
// terminal will send back as keystrokes: autowrap, cursor visibility, alternate screen, bracketed
// paste, application cursor keys and keypad, origin mode, reverse video.
func allowedMode(params string) bool {
	switch params {
	case "?1", "?5", "?6", "?7", "?12", "?25", "?47", "?1047", "?1048", "?1049", "?2004", "?2026":
		return true // (not ?1004, focus reporting: the pod has no need to know the owner's focus)
	}
	return false
}

// Modes are the private modes the filter lets through; a terminal reset must undo every one.
var Modes = []string{"?1", "?5", "?6", "?7", "?12", "?25", "?47", "?1047", "?1048", "?1049", "?2004", "?2026"}

// validNext reports whether c may be the byte at position pn (1..3) of a character that began with lead:
// a continuation byte, in the second-byte ranges that rule out overlong forms, surrogates and anything
// above U+10FFFF.
func validNext(lead byte, pn int, c byte) bool {
	lo, hi := byte(0x80), byte(0xbf)
	if pn == 1 {
		switch lead {
		case 0xe0:
			lo = 0xa0
		case 0xed:
			hi = 0x9f
		case 0xf0:
			lo = 0x90
		case 0xf4:
			hi = 0x8f
		}
	}
	return c >= lo && c <= hi
}

// Write filters p and returns the bytes to pass on. It never fails and never returns more than 2 bytes
// per input byte.
func (f *Filter) Write(p []byte) []byte {
	f.out = f.out[:0]
	for _, c := range p {
		f.step(c)
	}
	return append([]byte(nil), f.out...)
}

func (f *Filter) step(c byte) {
	switch f.st {
	case text:
		if f.need > 0 { // inside a multi-byte character
			if validNext(f.lead, f.pn, c) {
				f.pend[f.pn] = c
				f.pn++
				if f.pn-1 == f.need { // complete: UTF-8 is valid by construction; C1 controls (U+0080-U+009F) are dropped
					if !(f.pend[0] == 0xc2 && f.pend[1] >= 0x80 && f.pend[1] <= 0x9f) {
						f.out = append(f.out, f.pend[:f.pn]...)
					}
					f.need, f.pn = 0, 0
				}
				return
			}
			f.need, f.pn = 0, 0 // cut short or invalid: the partial character is dropped, and this byte is looked at afresh
		}
		switch {
		case c == 0x1b:
			f.st = esc
		case c == 0x08 || c == 0x09 || c == 0x0a || c == 0x0b || c == 0x0c || c == 0x0d || c == 0x07:
			f.out = append(f.out, c)
		case c < 0x20 || c == 0x7f:
			// the other C0 controls (ENQ makes a terminal answer, SO/SI switch character sets) and DEL: dropped
		case c < 0x80:
			f.out = append(f.out, c)
		case c >= 0xc2 && c <= 0xdf:
			f.lead, f.need, f.pn, f.pend[0] = c, 1, 1, c
		case c >= 0xe0 && c <= 0xef:
			f.lead, f.need, f.pn, f.pend[0] = c, 2, 1, c
		case c >= 0xf0 && c <= 0xf4:
			f.lead, f.need, f.pn, f.pend[0] = c, 3, 1, c
		default:
			// 0x80-0xBF with no character open (raw C1 controls: CSI, OSC, DCS as single bytes in an 8-bit
			// terminal; or a stray continuation byte), 0xC0 and 0xC1 (overlong), 0xF5-0xFF: never valid
		}
	case esc:
		switch {
		case c == '[':
			f.st, f.buf = csi, f.buf[:0]
		case c == ']':
			f.st, f.n, f.osc = str, 0, true
		case c == 'P' || c == '_' || c == '^' || c == 'X':
			f.st, f.n, f.osc = str, 0, false
		case c >= 0x20 && c <= 0x2f:
			f.st = escInt // ESC ( B and friends: the character-set designations; the next byte is dropped too
		case c == '7' || c == '8' || c == 'M' || c == 'D' || c == 'E' || c == '=' || c == '>' || c == 'c':
			f.out = append(f.out, 0x1b, c) // save/restore cursor, index, reverse index, next line, keypad modes, full reset
			f.st = text
		case c == 0x1b:
			// ESC ESC: stay in esc
		default:
			f.st = text // Z (a query), \ (a stray ST) and the rest: dropped
		}
	case escInt:
		f.st = text
	case csi:
		switch {
		case c >= 0x30 && c <= 0x3f, c >= 0x20 && c <= 0x2f: // parameters and intermediates
			if len(f.buf) >= 64 {
				f.st = text // a CSI that long is not drawing anything
				return
			}
			f.buf = append(f.buf, c)
		case c >= 0x40 && c <= 0x7e:
			f.st = text
			f.endCSI(c)
		case c == 0x1b:
			f.st = esc // an interrupted CSI is abandoned and a new sequence begins
		case c < 0x20:
			// a control inside a CSI executes on a real terminal; here it is dropped, the sequence goes on
		default:
			f.st = text
		}
	case str:
		f.n++
		switch {
		case c == 0x07 && f.osc:
			f.st = text
		case c == 0x1b:
			f.st = strEsc
		case c == 0x9c:
			f.st = text
		case f.n > maxString:
			f.st = text
		}
	case strEsc:
		if c == '\\' {
			f.st = text
		} else if c == 0x1b {
			// stay
		} else {
			f.st = str
			f.n++
		}
	}
}

// endCSI passes the sequence if it only draws.
func (f *Filter) endCSI(final byte) {
	params := string(f.buf)
	ok := false
	switch final {
	case 'h', 'l':
		ok = allowedMode(params)
	case 'm':
		// SGR takes digits, ; and : only. A private marker or an intermediate makes it something else.
		ok = onlyDigits(params)
	case 'q':
		ok = false
	default:
		ok = contains(csiFinal, final) && onlyDigits(params)
	}
	if !ok {
		return
	}
	f.out = append(f.out, 0x1b, '[')
	f.out = append(f.out, f.buf...)
	f.out = append(f.out, final)
}

func onlyDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && c != ';' && c != ':' {
			return false
		}
	}
	return true
}

func contains(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}

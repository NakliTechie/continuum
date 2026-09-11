package cli

import "unicode/utf8"

// textFilter reduces a raw PTY byte stream to what a person can safely see in
// their own terminal: printable text, line structure, and colour (numeric
// SGR). Every other control function is dropped — clipboard and title OSCs,
// device queries whose answers would land on the shell's stdin, alternate
// screen and paste-mode toggles, cursor and margin controls, DCS/APC strings.
// It is stream-safe: an escape split across two output events is finished on
// the next write, so `events --text` can follow live output.
type textFilter struct {
	state   byte   // 0 ground, 1 ESC, 2 CSI, 3 string (OSC/DCS/APC/PM/SOS), 4 string saw ESC, 5 ESC intermediate
	pending []byte // the CSI bytes seen so far, kept only while they could still be SGR
	utf8    []byte // an incomplete multi-byte character, kept across writes
	need    int    // continuation bytes still expected for utf8
}

const maxCSI = 64 // an SGR line longer than this is not one we render

// replacement stands in for bytes that are not valid UTF-8, including lone
// 8-bit C1 controls (0x80–0x9f), which a terminal accepting 8-bit controls
// would otherwise read as CSI, OSC or ST.
const replacement = "\uFFFD"

func (f *textFilter) Write(p []byte) []byte {
	out := make([]byte, 0, len(p))
	for _, b := range p {
		if f.state == 0 && f.need > 0 {
			if b >= 0x80 && b <= 0xbf {
				f.utf8 = append(f.utf8, b)
				if f.need--; f.need == 0 {
					// Structure is not enough: an overlong form, a surrogate
					// or a value past U+10FFFF is rejected by a real decoder,
					// after which its bytes are loose again — one of them a
					// C1 control, in the worst case.
					if _, n := utf8.DecodeRune(f.utf8); n == len(f.utf8) {
						out = append(out, f.utf8...)
					} else {
						out = append(out, replacement...)
					}
					f.utf8 = f.utf8[:0]
				}
				continue
			}
			// The sequence broke off: what arrived is not a character.
			out = append(out, replacement...)
			f.utf8, f.need = f.utf8[:0], 0
		}
		switch f.state {
		case 0:
			switch {
			case b == 0x1b:
				f.state = 1
			case b == '\n' || b == '\r' || b == '\t':
				out = append(out, b)
			case b < 0x20 || b == 0x7f:
				// other C0 controls (BEL, BS, FF, VT, SO/SI ...) are dropped
			case b < 0x80:
				out = append(out, b)
			case b >= 0xc2 && b <= 0xdf:
				f.utf8, f.need = append(f.utf8[:0], b), 1
			case b >= 0xe0 && b <= 0xef:
				f.utf8, f.need = append(f.utf8[:0], b), 2
			case b >= 0xf0 && b <= 0xf4:
				f.utf8, f.need = append(f.utf8[:0], b), 3
			default:
				// a stray continuation byte, an overlong lead, or a C1 control
				out = append(out, replacement...)
			}
		case 1: // after ESC
			switch {
			case b == '[':
				f.state, f.pending = 2, f.pending[:0]
			case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
				f.state = 3
			case b >= 0x20 && b <= 0x2f:
				f.state = 5
			default: // two-byte ESC sequence (RIS, DECSC, charset ...) — dropped
				f.state = 0
			}
		case 5: // ESC intermediate(s) then a final byte
			if b >= 0x30 && b <= 0x7e {
				f.state = 0
			}
		case 2: // CSI parameters/intermediates then a final byte
			switch {
			case b >= 0x40 && b <= 0x7e:
				if b == 'm' && sgrParams(f.pending) {
					out = append(out, 0x1b, '[')
					out = append(out, f.pending...)
					out = append(out, 'm')
				}
				f.state, f.pending = 0, f.pending[:0]
			case len(f.pending) < maxCSI:
				f.pending = append(f.pending, b)
			default:
				f.pending = append(f.pending[:0], 0xff) // poisoned: never SGR
			}
		case 3: // control string: ends at BEL or ESC \
			if b == 0x07 {
				f.state = 0
			} else if b == 0x1b {
				f.state = 4
			}
		case 4: // ESC inside a control string: ST ends it, BEL still ends it
			switch b {
			case '\\', 0x07:
				f.state = 0
			case 0x1b:
			default:
				f.state = 3
			}
		}
	}
	return out
}

func sgrParams(p []byte) bool {
	for _, c := range p {
		if !((c >= '0' && c <= '9') || c == ';' || c == ':') {
			return false
		}
	}
	return true
}

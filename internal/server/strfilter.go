package server

// strFilter scrubs non-ASCII bytes from the payload of escape-introduced
// control strings (OSC / DCS / APC / PM / SOS) before child output reaches
// the x/vt emulator.
//
// x/ansi's parser treats 0x9C as the C1 String Terminator even inside an
// OSC, but in UTF-8 that byte is an ordinary continuation byte. Claude
// Code sets the window title to "✳ <summary>" and "✳" is E2 9C B3, so the
// title was cut short and the rest of it got printed onto the screen —
// once per spinner frame. Dropping bytes >= 0x80 inside these strings
// keeps the parser in sync; the only loss is non-ASCII title / hyperlink
// text, which the emulator never displays anyway.
//
// State carries across Write calls because a sequence can straddle two
// PTY reads. Only the emulator path is filtered: a fullscreen raw client
// still gets the child's bytes verbatim.
type strFilter struct {
	state strFilterState
	buf   []byte
}

type strFilterState uint8

const (
	sfGround strFilterState = iota
	sfEsc                   // saw ESC in ground
	sfStr                   // inside OSC/DCS/APC/PM/SOS payload
	sfStrEsc                // saw ESC inside a string (maybe ESC \ = ST)
)

const (
	bEsc = 0x1b
	bBel = 0x07
	bCan = 0x18
	bSub = 0x1a
)

// filter returns p with in-string non-ASCII bytes removed. The returned
// slice aliases an internal buffer that is reused by the next call.
func (f *strFilter) filter(p []byte) []byte {
	f.buf = f.buf[:0]
	for _, b := range p {
		switch f.state {
		case sfGround:
			if b == bEsc {
				f.state = sfEsc
			}
		case sfEsc:
			f.state = escNext(b)
		case sfStr:
			switch {
			case b == bEsc:
				f.state = sfStrEsc
			case b == bBel || b == bCan || b == bSub:
				f.state = sfGround
			case b >= 0x80:
				continue
			}
		case sfStrEsc:
			if b == '\\' {
				f.state = sfGround
			} else {
				// ESC + anything else aborts the string and starts a new
				// escape sequence in the parser; follow it.
				f.state = escNext(b)
			}
		}
		f.buf = append(f.buf, b)
	}
	return f.buf
}

// escNext is the state after ESC followed by b.
func escNext(b byte) strFilterState {
	switch b {
	case ']', 'P', '_', '^', 'X':
		return sfStr
	case bEsc:
		return sfEsc
	}
	return sfGround
}

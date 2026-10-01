package desktop

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// keymap is the server's keyboard map, inverted: which keycode makes a
// keysym, and whether it needs Shift.
type keymap struct {
	perCode byte
	min     xproto.Keycode
	syms    map[xproto.Keysym]keyAt
	// A keycode with no keysyms at all, for typing what the map does not
	// have. Zero when there is none.
	spare xproto.Keycode
	shift xproto.Keycode
}

type keyAt struct {
	code  xproto.Keycode
	shift bool
}

func readKeymap(conn *xgb.Conn, minCode, maxCode xproto.Keycode) (keymap, error) {
	count := int(maxCode) - int(minCode) + 1
	r, err := xproto.GetKeyboardMapping(conn, minCode, byte(count)).Reply()
	if err != nil {
		return keymap{}, err
	}
	return buildKeymap(minCode, r.KeysymsPerKeycode, r.Keysyms), nil
}

// buildKeymap is readKeymap's arithmetic, apart so it can be tested without
// a server.
func buildKeymap(minCode xproto.Keycode, per byte, syms []xproto.Keysym) keymap {
	km := keymap{perCode: per, min: minCode, syms: map[xproto.Keysym]keyAt{}}
	n := int(per)
	if n == 0 {
		return km
	}
	for i := 0; i*n < len(syms); i++ {
		code := xproto.Keycode(int(minCode) + i)
		group := syms[i*n : min(len(syms), (i+1)*n)]
		empty := true
		for col, s := range group {
			if s == 0 {
				continue
			}
			empty = false
			// Only the first group's two columns: unshifted and shifted. The
			// rest are other layouts' and AltGr levels, which a tap with or
			// without Shift does not reach.
			if col > 1 {
				continue
			}
			if _, seen := km.syms[s]; !seen {
				km.syms[s] = keyAt{code: code, shift: col == 1}
			}
		}
		// The highest unused keycode, as xdotool picks: low ones are where a
		// layout switch is most likely to put something.
		if empty && code > 8 {
			km.spare = code
		}
	}
	if at, ok := km.syms[symShiftL]; ok {
		km.shift = at.code
	}
	// A letter's lowercase keysym is in column 0 and its uppercase is often
	// implicit (column 1 empty): X derives it. Record uppercase as the same
	// key with Shift, so "A" types as Shift+a rather than through the spare.
	for s, at := range km.syms {
		if s >= 'a' && s <= 'z' && !at.shift {
			up := s - 'a' + 'A'
			if _, ok := km.syms[up]; !ok {
				km.syms[up] = keyAt{code: at.code, shift: true}
			}
		}
	}
	return km
}

func (km keymap) lookup(sym xproto.Keysym) (xproto.Keycode, bool, bool) {
	at, ok := km.syms[sym]
	return at.code, at.shift, ok
}

func tapKey(conn *xgb.Conn, root xproto.Window, code xproto.Keycode, shift bool, km keymap) error {
	if shift && km.shift != 0 {
		if err := fake(conn, root, xproto.KeyPress, byte(km.shift), 0, 0); err != nil {
			return err
		}
	}
	if err := fake(conn, root, xproto.KeyPress, byte(code), 0, 0); err != nil {
		return err
	}
	if err := fake(conn, root, xproto.KeyRelease, byte(code), 0, 0); err != nil {
		return err
	}
	if shift && km.shift != 0 {
		return fake(conn, root, xproto.KeyRelease, byte(km.shift), 0, 0)
	}
	return nil
}

// Keysyms with names, the ones an agent and a browser ask for.
const (
	symShiftL = 0xffe1
)

var namedKeys = map[string]xproto.Keysym{
	"return": 0xff0d, "enter": 0xff0d,
	"tab":    0xff09,
	"escape": 0xff1b, "esc": 0xff1b,
	"backspace": 0xff08,
	"delete":    0xffff, "del": 0xffff,
	"insert": 0xff63,
	"home":   0xff50, "end": 0xff57,
	"left": 0xff51, "up": 0xff52, "right": 0xff53, "down": 0xff54,
	"arrowleft": 0xff51, "arrowup": 0xff52, "arrowright": 0xff53, "arrowdown": 0xff54,
	"page_up": 0xff55, "pageup": 0xff55, "prior": 0xff55,
	"page_down": 0xff56, "pagedown": 0xff56, "next": 0xff56,
	"space": 0x20,
	"shift": symShiftL, "shift_l": symShiftL, "shift_r": 0xffe2,
	"ctrl": 0xffe3, "control": 0xffe3, "control_l": 0xffe3, "control_r": 0xffe4,
	"alt": 0xffe9, "alt_l": 0xffe9, "alt_r": 0xffea, "option": 0xffe9,
	"super": 0xffeb, "super_l": 0xffeb, "win": 0xffeb, "meta": 0xffeb, "cmd": 0xffeb,
	"caps_lock": 0xffe5, "capslock": 0xffe5,
	"print": 0xff61, "menu": 0xff67,
	"f1": 0xffbe, "f2": 0xffbf, "f3": 0xffc0, "f4": 0xffc1, "f5": 0xffc2, "f6": 0xffc3,
	"f7": 0xffc4, "f8": 0xffc5, "f9": 0xffc6, "f10": 0xffc7, "f11": 0xffc8, "f12": 0xffc9,
}

// KeysymByName resolves a key name as a browser's KeyboardEvent.key or an
// agent writes it: a named key, or a single character.
func KeysymByName(name string) (xproto.Keysym, bool) {
	if s, ok := namedKeys[strings.ToLower(name)]; ok {
		return s, true
	}
	r := []rune(name)
	if len(r) == 1 {
		return runeKeysym(r[0]), true
	}
	return 0, false
}

// runeKeysym is the keysym for a character: Latin-1 is its own code, the
// rest of Unicode is 0x01000000 plus the code point, by the X11 convention.
func runeKeysym(r rune) xproto.Keysym {
	switch r {
	case '\n', '\r':
		return 0xff0d
	case '\t':
		return 0xff09
	}
	if (r >= 0x20 && r <= 0x7e) || (r >= 0xa0 && r <= 0xff) {
		return xproto.Keysym(r)
	}
	return xproto.Keysym(0x01000000 + uint32(r))
}

// parseCombo reads "ctrl+shift+t", "Return", "alt+F4": names joined with
// "+", the last one tapped and the others held. A literal "+" is "plus".
func parseCombo(combo string) ([]xproto.Keysym, error) {
	combo = strings.TrimSpace(combo)
	if combo == "" {
		return nil, fmt.Errorf("desktop: an empty key")
	}
	parts := strings.Split(combo, "+")
	out := make([]xproto.Keysym, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.EqualFold(p, "plus") {
			p = "+"
		}
		if p == "" {
			return nil, fmt.Errorf("desktop: %q has an empty part", combo)
		}
		s, ok := KeysymByName(p)
		if !ok {
			return nil, fmt.Errorf("desktop: no key called %q in %q", p, combo)
		}
		// A modifier combination with a letter means the letter key, not the
		// uppercase character: ctrl+L is ctrl+l, the way people write it.
		if len(parts) > 1 && len([]rune(p)) == 1 && unicode.IsUpper([]rune(p)[0]) {
			s = runeKeysym(unicode.ToLower([]rune(p)[0]))
		}
		out = append(out, s)
	}
	return out, nil
}

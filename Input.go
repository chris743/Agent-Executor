//go:build windows

package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002
	keyeventfUnicode     = 0x0004
	keyeventfScanCode    = 0x0008

	meMove        = 0x0001
	meLeftDown    = 0x0002
	meLeftUp      = 0x0004
	meRightDown   = 0x0008
	meRightUp     = 0x0010
	meMiddleDown  = 0x0020
	meMiddleUp    = 0x0040
	meVirtualDesk = 0x4000
	meAbsolute    = 0x8000

	vkReturn = 0x0D
	vkTab    = 0x09
	vkLShift = 0xA0

	keyEventGap = 8 * time.Millisecond
)

// Struct layouts match the 64-bit Windows INPUT union (40 bytes).
type keybdInput struct {
	Vk        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type keyboardINPUT struct {
	Type uint32
	Ki   keybdInput
	_    [8]byte
}

type mouseInput struct {
	Dx, Dy    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type mouseINPUT struct {
	Type uint32
	Mi   mouseInput
}

func init() {
	if unsafe.Sizeof(keyboardINPUT{}) != 40 || unsafe.Sizeof(mouseINPUT{}) != 40 {
		panic("erp-executor must be built for 64-bit Windows (GOARCH=amd64 or arm64)")
	}
}

// Keys that need KEYEVENTF_EXTENDEDKEY, otherwise RDP maps them to the numpad.
var extendedVK = map[uint16]bool{
	0x2D: true, 0x2E: true, 0x24: true, 0x23: true, 0x21: true, 0x22: true, // ins del home end pgup pgdn
	0x25: true, 0x26: true, 0x27: true, 0x28: true, // arrows
	0xA3: true, 0xA5: true, 0x5B: true, 0x5C: true, 0x5D: true, // rctrl ralt lwin rwin apps
	0x6F: true, 0x90: true, 0x2C: true, // numpad divide, numlock, printscreen
}

var modifierVK = map[string]uint16{"ctrl": 0xA2, "shift": 0xA0, "alt": 0xA4, "win": 0x5B}
var modifierOrder = []string{"ctrl", "shift", "alt", "win"}

var keyAliases = map[string]string{
	"control": "ctrl", "return": "enter", "escape": "esc", "del": "delete",
	"ins": "insert", "pageup": "pgup", "pagedown": "pgdn", "bksp": "backspace",
}

var namedVK = map[string]uint16{
	"enter": 0x0D, "tab": 0x09, "esc": 0x1B, "space": 0x20, "backspace": 0x08,
	"delete": 0x2E, "insert": 0x2D, "home": 0x24, "end": 0x23, "pgup": 0x21, "pgdn": 0x22,
	"up": 0x26, "down": 0x28, "left": 0x25, "right": 0x27, "apps": 0x5D,
	"comma": 0xBC, "period": 0xBE, "minus": 0xBD, "plus": 0xBB,
}

func init() {
	for c := 'a'; c <= 'z'; c++ {
		namedVK[string(c)] = uint16('A' + (c - 'a'))
	}
	for c := '0'; c <= '9'; c++ {
		namedVK[string(c)] = uint16(c)
	}
	for i := 1; i <= 24; i++ {
		namedVK[fmt.Sprintf("f%d", i)] = uint16(0x70 + i - 1)
	}
}

func canonical(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if a, ok := keyAliases[name]; ok {
		return a
	}
	return name
}

// chord is one key press with optional modifiers, e.g. "ctrl+shift+s" or "f8".
type chord struct {
	mods   []string
	key    string
	modVKs []uint16
	keyVK  uint16
}

// String returns the canonical form (fixed modifier order), used for allowlists.
func (c chord) String() string {
	return strings.Join(append(append([]string(nil), c.mods...), c.key), "+")
}

func parseChord(s string) (chord, error) {
	parts := strings.Split(s, "+")
	keyName := canonical(parts[len(parts)-1])
	if keyName == "" {
		return chord{}, fmt.Errorf("invalid key chord %q", s)
	}
	set := map[string]bool{}
	for _, p := range parts[:len(parts)-1] {
		m := canonical(p)
		if _, ok := modifierVK[m]; !ok {
			return chord{}, fmt.Errorf("%q in %q is not a modifier (ctrl, shift, alt, win)", p, s)
		}
		set[m] = true
	}
	c := chord{key: keyName}
	for _, m := range modifierOrder {
		if set[m] {
			c.mods = append(c.mods, m)
			c.modVKs = append(c.modVKs, modifierVK[m])
		}
	}
	if vk, ok := namedVK[keyName]; ok {
		c.keyVK = vk
	} else if vk, ok := modifierVK[keyName]; ok {
		c.keyVK = vk // a bare "alt" to open the menu bar
	} else {
		return chord{}, fmt.Errorf("unknown key %q in %q", keyName, s)
	}
	return c, nil
}

func (c chord) events() []keybdInput {
	var ev []keybdInput
	for _, vk := range c.modVKs {
		ev = append(ev, vkEvent(vk, false))
	}
	ev = append(ev, vkEvent(c.keyVK, false), vkEvent(c.keyVK, true))
	for i := len(c.modVKs) - 1; i >= 0; i-- {
		ev = append(ev, vkEvent(c.modVKs[i], true))
	}
	return ev
}

// vkEvent sends hardware scan codes. The RDP client forwards scan codes to the
// server, so VK-only injected events are often dropped or mistranslated.
func vkEvent(vk uint16, up bool) keybdInput {
	sc, _, _ := pMapVirtualKeyW.Call(uintptr(vk), 0) // MAPVK_VK_TO_VSC
	e := keybdInput{Vk: vk, Scan: uint16(sc)}
	if sc != 0 {
		e.Flags |= keyeventfScanCode
	}
	if extendedVK[vk] {
		e.Flags |= keyeventfExtendedKey
	}
	if up {
		e.Flags |= keyeventfKeyUp
	}
	return e
}

func tap(vk uint16) []keybdInput {
	return []keybdInput{vkEvent(vk, false), vkEvent(vk, true)}
}

// charEvents types one character. In "keys" mode it uses the client keyboard
// layout's physical key (+shift); characters needing AltGr/Ctrl, or anything not
// on the layout, fall back to Unicode events.
func charEvents(r rune, unicodeOnly bool) []keybdInput {
	if !unicodeOnly && r <= 0xFFFF {
		v, _, _ := pVkKeyScanW.Call(uintptr(r))
		res := int16(v)
		if res != -1 {
			vk := uint16(res & 0xFF)
			state := (res >> 8) & 0xFF
			if state&^1 == 0 {
				if state&1 != 0 {
					return []keybdInput{vkEvent(vkLShift, false), vkEvent(vk, false),
						vkEvent(vk, true), vkEvent(vkLShift, true)}
				}
				return tap(vk)
			}
		}
	}
	var ev []keybdInput
	for _, u := range utf16.Encode([]rune{r}) {
		ev = append(ev,
			keybdInput{Scan: u, Flags: keyeventfUnicode},
			keybdInput{Scan: u, Flags: keyeventfUnicode | keyeventfKeyUp})
	}
	return ev
}

func sendKeyboardSeq(ev []keybdInput) error {
	for i := range ev {
		in := keyboardINPUT{Type: inputKeyboard, Ki: ev[i]}
		n, _, err := pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
		if n != 1 {
			return fmt.Errorf("SendInput was blocked (locked session, UAC prompt, or elevated target?): %v", err)
		}
		time.Sleep(keyEventGap)
	}
	return nil
}

func typeText(text string, unicodeOnly bool, perChar time.Duration, guard func() error) error {
	runes := []rune(text)
	for i, r := range runes {
		if err := guard(); err != nil {
			return progress(err, i, len(runes))
		}
		var ev []keybdInput
		switch r {
		case '\r':
			continue
		case '\n':
			ev = tap(vkReturn)
		case '\t':
			ev = tap(vkTab)
		default:
			ev = charEvents(r, unicodeOnly)
		}
		if err := sendKeyboardSeq(ev); err != nil {
			return progress(err, i, len(runes))
		}
		time.Sleep(perChar)
	}
	return nil
}

func sendMouse(mi mouseInput) error {
	in := mouseINPUT{Type: inputMouse, Mi: mi}
	n, _, err := pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	if n != 1 {
		return fmt.Errorf("SendInput (mouse) was blocked: %v", err)
	}
	return nil
}

// clickAt moves with a real input event first (so the RDP client forwards the
// position to the server) and then clicks. x, y are physical screen pixels.
func clickAt(x, y int, button string, double bool) error {
	vs := virtualScreen()
	nx := int32(int64(x-vs.X) * 65535 / int64(maxInt(vs.W-1, 1)))
	ny := int32(int64(y-vs.Y) * 65535 / int64(maxInt(vs.H-1, 1)))
	if err := sendMouse(mouseInput{Dx: nx, Dy: ny, Flags: meMove | meAbsolute | meVirtualDesk}); err != nil {
		return err
	}
	time.Sleep(80 * time.Millisecond)

	down, up := uint32(meLeftDown), uint32(meLeftUp)
	switch button {
	case "right":
		down, up = meRightDown, meRightUp
	case "middle":
		down, up = meMiddleDown, meMiddleUp
	}
	clicks := 1
	if double {
		clicks = 2
	}
	for i := 0; i < clicks; i++ {
		if i > 0 {
			time.Sleep(70 * time.Millisecond)
		}
		if err := sendMouse(mouseInput{Flags: down}); err != nil {
			return err
		}
		time.Sleep(40 * time.Millisecond)
		if err := sendMouse(mouseInput{Flags: up}); err != nil {
			return err
		}
	}
	return nil
}
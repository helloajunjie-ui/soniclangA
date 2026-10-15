//go:build windows

package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"
)

// The Win32 INPUT structure ends in a C union, and SendInput rejects any cbSize
// that differs from sizeof(INPUT). Modelling the union as two sequential Go
// fields produced a 64-byte struct where the API expects 40, so every click and
// keystroke failed at runtime while the code looked right and compiled cleanly.
// These assertions pin the layout against the documented ABI so that mistake
// cannot come back unnoticed.
func TestInputLayoutMatchesWin32ABI(t *testing.T) {
	ptr := unsafe.Sizeof(uintptr(0))

	// MOUSEINPUT and KEYBDINPUT sizes. The union takes the larger one, so
	// MOUSEINPUT is what the struct must be able to hold.
	wantKeybd, wantMouse := uintptr(24), uintptr(32)
	if ptr == 4 {
		wantKeybd, wantMouse = 16, 24
	}
	if got := unsafe.Sizeof(keybdInput{}); got != wantKeybd {
		t.Errorf("sizeof(KEYBDINPUT) = %d, want %d", got, wantKeybd)
	}
	if got := unsafe.Sizeof(mouseInput{}); got != wantMouse {
		t.Errorf("sizeof(MOUSEINPUT) = %d, want %d", got, wantMouse)
	}
	if unsafe.Sizeof(keybdInput{}) > unsafe.Sizeof(mouseInput{}) {
		t.Error("the union is sized from MOUSEINPUT, which is no longer the larger variant")
	}

	// sizeof(INPUT) = type (DWORD) + padding to pointer alignment + the union.
	wantInput := wantMouse + uintptr(4)
	if ptr == 8 {
		wantInput = wantMouse + 8
	}
	if got := unsafe.Sizeof(input{}); got != wantInput {
		t.Fatalf("sizeof(INPUT) = %d, want %d — SendInput will reject this cbSize", got, wantInput)
	}
	if off := unsafe.Offsetof(input{}.union); off != ptr {
		t.Errorf("union offset = %d, want %d (the ABI packs it on a pointer boundary)", off, ptr)
	}
	if off := unsafe.Offsetof(input{}.Type); off != 0 {
		t.Errorf("Type offset = %d, want 0", off)
	}
}

// A keybdInput written into the union must read back unchanged, and a mouse
// input must not be disturbed by a preceding keyboard one — the two share
// storage, so a leftover field would silently ride along into the OS call.
func TestInputUnionAccessors(t *testing.T) {
	kb := kbInput(keybdInput{Vk: 0x53, Scan: 0x1F, Flags: keyEventFUnicode})
	if kb.Type != inputKeyboard {
		t.Errorf("keyboard INPUT type = %d, want %d", kb.Type, inputKeyboard)
	}
	back := *(*keybdInput)(unsafe.Pointer(&kb.union[0]))
	if back.Vk != 0x53 || back.Scan != 0x1F || back.Flags != keyEventFUnicode {
		t.Errorf("KEYBDINPUT did not survive the union write: %+v", back)
	}

	ms := msInput(mouseInput{Dx: 100, Dy: 200, Flags: mouseEventFAbsolute})
	if ms.Type != inputMouse {
		t.Errorf("mouse INPUT type = %d, want %d", ms.Type, inputMouse)
	}
	mback := *(*mouseInput)(unsafe.Pointer(&ms.union[0]))
	if mback.Dx != 100 || mback.Dy != 200 || mback.Flags != mouseEventFAbsolute {
		t.Errorf("MOUSEINPUT did not survive the union write: %+v", mback)
	}
}

// A shortcut is "all modifiers held while the main key fires", not a sequence
// of independent keystrokes. Pinning the event order here is the only way to
// check it without synthesising real input on the user's desktop.
func TestHotkeyInputsHoldKeysTogether(t *testing.T) {
	codes := []uint16{vkControl, 0x53} // ctrl + s
	ins := hotkeyInputs(codes)

	if len(ins) != len(codes)*2 {
		t.Fatalf("got %d events, want %d (one down and one up per key)", len(ins), len(codes)*2)
	}
	// Every event must be a keyboard event of the right shape.
	for i, in := range ins {
		if in.Type != inputKeyboard {
			t.Fatalf("event %d is not a keyboard event", i)
		}
	}
	// Down phase: all keys down, in order, none released yet.
	for i, want := range codes {
		ki := *(*keybdInput)(unsafe.Pointer(&ins[i].union[0]))
		if ki.Vk != want || ki.Flags&keyEventFKeyUp != 0 {
			t.Fatalf("event %d should be a key-down of %#x, got %+v", i, want, ki)
		}
	}
	// Up phase: released in reverse, so nothing is still held at the end.
	for i, want := range []uint16{0x53, vkControl} {
		ki := *(*keybdInput)(unsafe.Pointer(&ins[len(codes)+i].union[0]))
		if ki.Vk != want || ki.Flags&keyEventFKeyUp == 0 {
			t.Fatalf("event %d should be a key-up of %#x, got %+v", len(codes)+i, want, ki)
		}
	}
}

// Truncating a rune into a uint16 would send a lone surrogate for anything
// outside the BMP, which the target renders as garbage.
func TestTextUnitsKeepsSurrogatePairs(t *testing.T) {
	if got := len(textUnits("ab")); got != 2 {
		t.Errorf("ASCII text produced %d units, want 2", got)
	}
	// CJK is inside the BMP: one unit per character.
	if got := len(textUnits("中文")); got != 2 {
		t.Errorf("BMP text produced %d units, want 2", got)
	}
	// An emoji needs a surrogate pair.
	units := textUnits("😀")
	if len(units) != 2 {
		t.Fatalf("an astral character produced %d units, want 2 (surrogate pair)", len(units))
	}
	for _, u := range units {
		if u < 0xD800 || u > 0xDFFF {
			t.Fatalf("unit %#x is not part of a surrogate pair", u)
		}
	}
}

// hwndRef/parseHwnd must round-trip, and parseHwnd must refuse anything it did
// not mint — an arbitrary integer would otherwise address a window the
// enumeration never offered.
func TestHwndRefRoundTripRejectsForeignIDs(t *testing.T) {
	h := uintptr(0x1234)
	back, err := parseHwnd(hwndRef(h))
	if err != nil || back != h {
		t.Fatalf("round trip failed: %v (got %#x, want %#x)", err, back, h)
	}
	for _, bad := range []string{"", "1234", "h:", "h:0", "h:abc", "h:1234x"} {
		if _, err := parseHwnd(bad); err == nil {
			t.Errorf("parseHwnd(%q) accepted a foreign id", bad)
		}
	}
}

// The process name is what most of the deny list matches on: terminals,
// password managers, task managers and LSA are named by executable, not by
// window class. A QueryFullProcessImageNameW call with the wrong argument shape
// fails with a plain zero return, which reads as "this window has no process" —
// the list looks present in the code and does nothing at runtime, and the
// console shows a blank column. Resolving our own pid is the cheapest way to
// pin the ABI.
func TestProcessNameResolvesTheCurrentExecutable(t *testing.T) {
	got := processName(uint32(os.Getpid()))
	if got == "" {
		t.Fatal("processName returned nothing for our own pid: every deny-list rule that matches on a process name would be inert")
	}
	if strings.ContainsAny(got, `\/`) {
		t.Errorf("processName returned %q, expected a bare executable name", got)
	}
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot determine our own executable: %v", err)
	}
	if want := filepath.Base(self); !strings.EqualFold(got, want) {
		t.Errorf("processName = %q, want %q", got, want)
	}
}

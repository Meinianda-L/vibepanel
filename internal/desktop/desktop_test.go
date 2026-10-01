package desktop

import (
	"image"
	"os"
	"testing"

	"github.com/jezek/xgb/xproto"
)

func TestCombosParseTheWayPeopleWriteThem(t *testing.T) {
	cases := map[string][]xproto.Keysym{
		"Return":       {0xff0d},
		"enter":        {0xff0d},
		"ctrl+l":       {0xffe3, 'l'},
		"ctrl+L":       {0xffe3, 'l'}, // a letter under a modifier is the key, not the capital
		"ctrl+shift+t": {0xffe3, 0xffe1, 't'},
		"alt+F4":       {0xffe9, 0xffc1},
		"super":        {0xffeb},
		"ctrl+plus":    {0xffe3, '+'},
		"A":            {'A'},
		"好":            {0x01000000 + '好'},
	}
	for in, want := range cases {
		got, err := parseCombo(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%q: %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q: %v, want %v", in, got, want)
			}
		}
	}
	for _, bad := range []string{"", "ctrl+", "ctrl+nosuchkey", "+"} {
		if _, err := parseCombo(bad); err == nil {
			t.Errorf("%q parsed; it should not", bad)
		}
	}
}

func TestKeymapFindsShiftedCharactersAndASpareKeycode(t *testing.T) {
	// Keycodes 8..12, two columns each: a/A, 1/!, Shift_L, nothing, nothing.
	syms := []xproto.Keysym{
		'a', 0,
		'1', '!',
		symShiftL, 0,
		0, 0,
		0, 0,
	}
	km := buildKeymap(8, 2, syms)
	if code, shift, ok := km.lookup('a'); !ok || code != 8 || shift {
		t.Errorf("a: %d %v %v", code, shift, ok)
	}
	if code, shift, ok := km.lookup('A'); !ok || code != 8 || !shift {
		t.Errorf("A should be Shift+a: %d %v %v", code, shift, ok)
	}
	if code, shift, ok := km.lookup('!'); !ok || code != 9 || !shift {
		t.Errorf("!: %d %v %v", code, shift, ok)
	}
	if km.shift != 10 {
		t.Errorf("shift keycode %d, want 10", km.shift)
	}
	if km.spare != 12 {
		t.Errorf("spare keycode %d, want the highest empty one, 12", km.spare)
	}
	if _, _, ok := km.lookup(runeKeysym('好')); ok {
		t.Error("a CJK character should not be on a US map; it goes through the spare")
	}
}

func TestPixelsComeOutInRGBOrder(t *testing.T) {
	// One pixel, blue=10 green=20 red=30, as an x86 server sends it.
	img := toRGBA([]byte{10, 20, 30, 0}, 1, 1, true)
	if p := img.Pix[:4]; p[0] != 30 || p[1] != 20 || p[2] != 10 || p[3] != 255 {
		t.Errorf("LSB: %v", p)
	}
	img = toRGBA([]byte{0, 30, 20, 10}, 1, 1, false)
	if p := img.Pix[:4]; p[0] != 30 || p[1] != 20 || p[2] != 10 {
		t.Errorf("MSB: %v", p)
	}
}

func TestResizeAveragesRatherThanSamples(t *testing.T) {
	// Alternating black and white columns: averaged to half width, every
	// pixel is mid grey. Point sampling would give all black or all white.
	src := image.NewRGBA(image.Rect(0, 0, 4, 1))
	for x := 0; x < 4; x++ {
		v := uint8(0)
		if x%2 == 1 {
			v = 255
		}
		copy(src.Pix[x*4:], []byte{v, v, v, 255})
	}
	dst := Resize(src, 2, 1)
	for x := 0; x < 2; x++ {
		if g := dst.Pix[x*4]; g < 120 || g > 135 {
			t.Errorf("pixel %d is %d, want about 128", x, g)
		}
	}
	if w, h := FitWidth(1920, 1080, 1280, 0); w != 1280 || h != 720 {
		t.Errorf("1920x1080 to 1280 wide: %dx%d", w, h)
	}
	if w, h := FitWidth(1920, 1080, 4000, 0); w != 1920 || h != 1080 {
		t.Errorf("never upscales: %dx%d", w, h)
	}
}

// TestAgainstARealDisplay runs only where a display is reachable and
// VIBEPANEL_DESKTOP_TEST names it -- never against somebody's session by
// accident, because it moves their pointer.
func TestAgainstARealDisplay(t *testing.T) {
	disp := os.Getenv("VIBEPANEL_DESKTOP_TEST")
	if disp == "" {
		t.Skip("VIBEPANEL_DESKTOP_TEST is not set")
	}
	d := Open(disp)
	w, h, err := d.Size()
	if err != nil {
		t.Fatal(err)
	}
	img, err := d.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if img.Rect.Dx() != w || img.Rect.Dy() != h {
		t.Fatalf("captured %v of a %dx%d screen", img.Rect, w, h)
	}
	if err := d.Move(ByAgent, w/2, h/2); err != nil {
		t.Fatal(err)
	}
	if x, y, _ := d.Pointer(); x != w/2 || y != h/2 {
		t.Errorf("pointer at %d,%d after moving to %d,%d", x, y, w/2, h/2)
	}
	d.Stop()
	if err := d.Move(ByAgent, 10, 10); err != ErrStopped {
		t.Errorf("an agent moved the pointer after Stop: %v", err)
	}
	if err := d.Move(ByPerson, 10, 10); err != nil {
		t.Errorf("the person was kept out: %v", err)
	}
	d.Resume()
	if err := d.Move(ByAgent, 20, 20); err != ErrPersonActive {
		t.Errorf("the agent got in straight after the person: %v", err)
	}
}

func TestMarkedTilesBecomeFewRectangles(t *testing.T) {
	g := newTiles(300, 200) // 5 x 4 tiles, the last column and row partial
	// An L: three tiles in row 0, then the first of those again in rows 1-2.
	g.markRect(0, 0, 192, 10)
	g.markRect(0, 64, 10, 128)
	got := g.rects(nil)
	want := []rect{{0, 0, 192, 64}, {0, 64, 64, 128}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rect %d: %v, want %v", i, got[i], want[i])
		}
	}
	// The partial tile at the edge stops at the screen, not at 64 pixels.
	g.clear()
	g.markRect(299, 199, 1, 1)
	if r := g.rects(nil); len(r) != 1 || r[0] != (rect{256, 192, 44, 8}) {
		t.Errorf("edge tile: %v", r)
	}
}

func TestScaleAveragesBlocksAndKeepsColourOrder(t *testing.T) {
	// 2x2 BGRX: red, red, blue, blue (as B,G,R,X).
	src := []byte{
		0, 0, 200, 0, 0, 0, 200, 0,
		200, 0, 0, 0, 200, 0, 0, 0,
	}
	img := scaleBGRX(src, 8, true, rect{0, 0, 2, 2}, 2)
	if p := img.Pix[:4]; p[0] != 100 || p[1] != 0 || p[2] != 100 {
		t.Errorf("2x2 average of red and blue: %v", p)
	}
	img = scaleBGRX(src, 8, true, rect{0, 0, 2, 2}, 1)
	if p := img.Pix[:4]; p[0] != 200 || p[2] != 0 {
		t.Errorf("unscaled red: %v", p)
	}
}

func TestScaleIsTheLargestThatStillMeetsTheWidth(t *testing.T) {
	for _, c := range []struct{ want, scale int }{{1920, 1}, {1200, 1}, {960, 2}, {600, 2}, {480, 4}, {200, 4}, {0, 1}} {
		if got := scaleFor(1920, c.want); got != c.scale {
			t.Errorf("want %d wide: scale %d, expected %d", c.want, got, c.scale)
		}
	}
}

func TestShotMatchesTheReferenceResize(t *testing.T) {
	// A 6x4 BGRX gradient reduced to 4x2 both ways: the fixed-point path must
	// agree with the float one to within a step of rounding.
	sw, sh := 6, 4
	src := make([]byte, sw*sh*4)
	for i := 0; i < sw*sh; i++ {
		src[i*4], src[i*4+1], src[i*4+2] = byte(i*7), byte(i*11), byte(i*13)
	}
	ref := Resize(toRGBA(src, sw, sh, true), 4, 2)
	got := ShotRGBA(src, sw, sh, true, 4, 2)
	for i := range ref.Pix {
		d := int(ref.Pix[i]) - int(got.Pix[i])
		if d < -1 || d > 1 {
			t.Fatalf("byte %d: fixed-point %d, float %d", i, got.Pix[i], ref.Pix[i])
		}
	}
}

func TestTwoThirdsMatchesTheGeneralBoxFilter(t *testing.T) {
	sw, sh := 12, 9
	src := make([]byte, sw*sh*4)
	for i := 0; i < sw*sh; i++ {
		src[i*4], src[i*4+1], src[i*4+2] = byte(i*37), byte(i*11+5), byte(255-i*3)
	}
	ref := Resize(toRGBA(src, sw, sh, true), 8, 6)
	got := shotTwoThirds(src, sw, 8, 6)
	for i := range ref.Pix {
		if d := int(ref.Pix[i]) - int(got.Pix[i]); d < -1 || d > 1 {
			t.Fatalf("byte %d: fast %d, reference %d", i, got.Pix[i], ref.Pix[i])
		}
	}
}

// Package desktop shows an X11 display in the panel and lets an agent
// operate it: screenshots, the pointer and the keyboard, with a person at the
// panel able to watch, take over, and stop the agent at any moment.
//
// It talks X11 directly (github.com/jezek/xgb, pure Go, so CGO_ENABLED=0
// still builds) as an ordinary client of a display on this machine, with the
// DISPLAY and XAUTHORITY of the user the panel runs as. That is the whole of
// its reach: it is not a proxy to anything, takes no address from a request,
// and stores no password. The retired VNC tab was a proxy to whatever address
// a row named, with the display's password in the clear in the database
// (docs/build-log.md, "The VNC tab"), and this was written to be none of
// that.
//
// Input goes through the XTEST extension, which is what xdotool uses; capture
// is GetImage on the root window. Both are in every X server a desktop Linux
// install has, and neither needs root.
package desktop

import (
	"errors"
	"fmt"
	"image"
	"strings"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// Who is asking. The person at the panel always wins over the agent.
type Source string

const (
	ByAgent  Source = "agent"
	ByPerson Source = "person"
)

// ErrStopped is the answer an agent gets after somebody pressed Stop.
var ErrStopped = errors.New("desktop: stopped by the person at the panel; ask them to resume")

// ErrPersonActive is the answer an agent gets while somebody is using the
// screen themselves. Two hands on one mouse is a fight the person should not
// have to win by speed.
var ErrPersonActive = errors.New("desktop: the person at the panel is using the screen; wait and take a screenshot")

// personGrace is how long after the person's last input the agent is kept
// off the screen.
const personGrace = 3 * time.Second

// Action is one thing done to the screen, kept for the panel to show (where
// the agent last clicked) and for the audit log.
type Action struct {
	By   Source    `json:"by"`
	Kind string    `json:"kind"`
	X    int       `json:"x,omitempty"`
	Y    int       `json:"y,omitempty"`
	Text string    `json:"text,omitempty"`
	At   time.Time `json:"at"`
}

// Status is what the panel shows about the desktop.
type Status struct {
	Display string  `json:"display"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Stopped bool    `json:"stopped"`
	Person  bool    `json:"person"`
	Last    *Action `json:"last,omitempty"`
	Viewers int     `json:"viewers"`
	Problem string  `json:"problem,omitempty"`
}

// Desktop is one X11 display.
type Desktop struct {
	display string

	mu       sync.Mutex
	conn     *xgb.Conn
	root     xproto.Window
	width    int
	height   int
	depth    byte
	bpp      byte
	lsb      bool
	keymap   keymap
	problem  string
	stopped  bool
	personAt time.Time
	last     *Action
	// Called after every action, outside the lock: the panel's audit log
	// and its live status.
	onAction func(Action)

	// The input lock, separate from mu so a long `type` does not stop a
	// screenshot or a Stop from being answered.
	input sync.Mutex

	// The screen as last captured, BGRX, stride width*4, and what the
	// viewers were last sent of it; both reused for the life of the
	// connection rather than allocated per frame. grabMu guards cur and the
	// grabber, which one capture at a time uses.
	grabMu  sync.Mutex
	cur     []byte
	prev    []byte
	grabber *shmGrabber
	// Damage: rectangles the server reported changed since the last frame.
	// Nil damaged means the extension is missing and the stream polls.
	damaged *damageQueue

	frames *streamHub
}

// Open connects to the display. A display that cannot be reached is not an
// error that stops the panel: it is a Desktop that says what is wrong and
// tries again on the next request, because the person's X session may simply
// not have started yet when the panel does.
func Open(display string) *Desktop {
	d := &Desktop{display: display}
	d.frames = newStreamHub(d)
	d.mu.Lock()
	d.connectLocked()
	d.mu.Unlock()
	return d
}

// OnAction sets the hook called after every action.
func (d *Desktop) OnAction(f func(Action)) {
	d.mu.Lock()
	d.onAction = f
	d.mu.Unlock()
}

func (d *Desktop) connectLocked() error {
	if d.conn != nil {
		return nil
	}
	conn, err := xgb.NewConnDisplay(d.display)
	if err != nil {
		d.problem = "cannot reach display " + d.display + ": " + err.Error()
		return errors.New(d.problem)
	}
	if err := xtest.Init(conn); err != nil {
		conn.Close()
		d.problem = "display " + d.display + " has no XTEST extension, so it cannot be operated: " + err.Error()
		return errors.New(d.problem)
	}
	setup := xproto.Setup(conn)
	screen := setup.DefaultScreen(conn)
	d.root = screen.Root
	d.width = int(screen.WidthInPixels)
	d.height = int(screen.HeightInPixels)
	d.depth = screen.RootDepth
	d.lsb = setup.ImageByteOrder == xproto.ImageOrderLSBFirst
	for _, f := range setup.PixmapFormats {
		if f.Depth == d.depth {
			d.bpp = f.BitsPerPixel
		}
	}
	if d.bpp != 32 {
		conn.Close()
		d.problem = fmt.Sprintf("display %s is %d bits per pixel; only 32 is supported", d.display, d.bpp)
		return errors.New(d.problem)
	}
	km, err := readKeymap(conn, setup.MinKeycode, setup.MaxKeycode)
	if err != nil {
		conn.Close()
		d.problem = "reading the keyboard map of " + d.display + ": " + err.Error()
		return errors.New(d.problem)
	}
	d.keymap = km
	d.conn = conn
	d.problem = ""

	d.grabMu.Lock()
	d.cur = make([]byte, d.width*d.height*4)
	d.prev = make([]byte, d.width*d.height*4)
	if d.grabber != nil {
		d.grabber.close()
		d.grabber = nil
	}
	// Shared memory when the server is on this machine, the socket otherwise.
	// With shared memory the screen buffer *is* the segment, so the server
	// writes captures straight into it.
	if g, err := newSHMGrabber(conn, d.root, d.width, d.height); err == nil {
		d.grabber = g
		d.cur = g.screen()
	}
	d.grabMu.Unlock()

	d.damaged = nil
	var watch *watcher
	if w, err := watchDamage(conn, d.root, conn.DefaultScreen); err == nil {
		watch = w
		d.damaged = w.q
	}
	// Ends when the connection does. See watch.go.
	go drainEvents(conn, watch)
	return nil
}

// dropLocked forgets a connection that failed, so the next call reconnects:
// an X server restarted by logging out and in again is a new server.
func (d *Desktop) dropLocked(err error) {
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
	d.problem = err.Error()
}

// Status is the desktop as the panel shows it.
func (d *Desktop) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		_ = d.connectLocked()
	}
	st := Status{
		Display: d.display,
		Width:   d.width,
		Height:  d.height,
		Stopped: d.stopped,
		Person:  time.Since(d.personAt) < personGrace,
		Last:    d.last,
		Viewers: d.frames.count(),
		Problem: d.problem,
	}
	return st
}

// Stop takes the screen away from the agent until Resume.
func (d *Desktop) Stop() {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
}

// Resume gives it back.
func (d *Desktop) Resume() {
	d.mu.Lock()
	d.stopped = false
	d.mu.Unlock()
}

// Size is the display's size in pixels, connecting if need be.
func (d *Desktop) Size() (int, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connectLocked(); err != nil {
		return 0, 0, err
	}
	return d.width, d.height, nil
}

// Shot grabs the whole screen and reduces it to fit maxW×maxH for the agent,
// in one pass from the raw buffer (see ShotRGBA).
func (d *Desktop) Shot(maxW, maxH int) (*image.RGBA, int, int, error) {
	d.mu.Lock()
	if err := d.connectLocked(); err != nil {
		d.mu.Unlock()
		return nil, 0, 0, err
	}
	w, h, lsb := d.width, d.height, d.lsb
	d.mu.Unlock()
	d.grabMu.Lock()
	defer d.grabMu.Unlock()
	if err := d.grabLocked(0, 0, w, h); err != nil {
		return nil, 0, 0, err
	}
	dw, dh := FitWidth(w, h, maxW, maxH)
	return ShotRGBA(d.cur, w, h, lsb, dw, dh), w, h, nil
}

// Capture grabs the whole screen as an image, for the agent: shared memory
// when there is some, converted once.
func (d *Desktop) Capture() (*image.RGBA, error) {
	d.mu.Lock()
	if err := d.connectLocked(); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	w, h, lsb := d.width, d.height, d.lsb
	d.mu.Unlock()

	d.grabMu.Lock()
	defer d.grabMu.Unlock()
	if err := d.grabLocked(0, 0, w, h); err != nil {
		return nil, err
	}
	return toRGBA(d.cur, w, h, lsb), nil
}

// grabLocked refreshes a rectangle of cur from the server. grabMu held.
func (d *Desktop) grabLocked(x, y, w, h int) error {
	d.mu.Lock()
	conn, root, sw := d.conn, d.root, d.width
	d.mu.Unlock()
	if conn == nil {
		return errors.New("desktop: not connected")
	}
	stride := sw * 4
	if d.grabber != nil {
		if err := d.grabber.grab(x, y, w, h, stride); err == nil {
			return nil
		}
		// A segment the server lost hold of: fall back for good rather than
		// failing every frame from here on, into a buffer of our own.
		old := d.cur
		d.grabber.close()
		d.grabber = nil
		d.cur = append([]byte(nil), old...)
	}
	reply, err := xproto.GetImage(conn, xproto.ImageFormatZPixmap, xproto.Drawable(root),
		int16(x), int16(y), uint16(w), uint16(h), 0xffffffff).Reply()
	if err != nil {
		d.mu.Lock()
		d.dropLocked(fmt.Errorf("capturing %s: %w", d.display, err))
		d.mu.Unlock()
		return err
	}
	row := w * 4
	for r := 0; r < h && (r+1)*row <= len(reply.Data); r++ {
		copy(d.cur[(y+r)*stride+x*4:(y+r)*stride+x*4+row], reply.Data[r*row:(r+1)*row])
	}
	return nil
}

// Pointer is where the pointer is now.
func (d *Desktop) Pointer() (int, int, error) {
	d.mu.Lock()
	if err := d.connectLocked(); err != nil {
		d.mu.Unlock()
		return 0, 0, err
	}
	conn, root := d.conn, d.root
	d.mu.Unlock()
	r, err := xproto.QueryPointer(conn, root).Reply()
	if err != nil {
		return 0, 0, err
	}
	return int(r.RootX), int(r.RootY), nil
}

// begin admits one action, or says why not. The person always gets in; the
// agent is kept out while stopped and while the person is using the screen.
func (d *Desktop) begin(by Source) (*xgb.Conn, xproto.Window, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if by == ByAgent {
		if d.stopped {
			return nil, 0, ErrStopped
		}
		if time.Since(d.personAt) < personGrace {
			return nil, 0, ErrPersonActive
		}
	} else {
		d.personAt = time.Now()
	}
	if err := d.connectLocked(); err != nil {
		return nil, 0, err
	}
	return d.conn, d.root, nil
}

func (d *Desktop) record(a Action) {
	a.At = time.Now()
	d.mu.Lock()
	d.last = &a
	f := d.onAction
	d.mu.Unlock()
	if f != nil {
		f(a)
	}
}

func (d *Desktop) clamp(x, y int) (int16, int16) {
	d.mu.Lock()
	w, h := d.width, d.height
	d.mu.Unlock()
	x = max(0, min(x, w-1))
	y = max(0, min(y, h-1))
	return int16(x), int16(y)
}

func fake(conn *xgb.Conn, root xproto.Window, typ byte, detail byte, x, y int16) error {
	return xtest.FakeInputChecked(conn, typ, detail, 0, root, x, y, 0).Check()
}

// Move puts the pointer at x, y.
func (d *Desktop) Move(by Source, x, y int) error {
	d.input.Lock()
	defer d.input.Unlock()
	conn, root, err := d.begin(by)
	if err != nil {
		return err
	}
	cx, cy := d.clamp(x, y)
	if err := fake(conn, root, xproto.MotionNotify, 0, cx, cy); err != nil {
		return err
	}
	d.record(Action{By: by, Kind: "move", X: int(cx), Y: int(cy)})
	return nil
}

// How a click is spaced; Click says why it is spaced at all. A triple click
// still ends well inside the 250ms double-click time.
const (
	clickSettle = 30 * time.Millisecond
	clickHold   = 40 * time.Millisecond
	clickGap    = 50 * time.Millisecond
)

// Button numbers as X has them.
const (
	ButtonLeft   = 1
	ButtonMiddle = 2
	ButtonRight  = 3
	wheelUp      = 4
	wheelDown    = 5
	wheelLeft    = 6
	wheelRight   = 7
)

// ButtonByName maps "left", "middle", "right" to X's numbers.
func ButtonByName(name string) (byte, error) {
	switch strings.ToLower(name) {
	case "", "left":
		return ButtonLeft, nil
	case "middle":
		return ButtonMiddle, nil
	case "right":
		return ButtonRight, nil
	}
	return 0, fmt.Errorf("desktop: no button %q; left, middle or right", name)
}

// Click moves to x, y and clicks count times.
func (d *Desktop) Click(by Source, x, y int, button byte, count int) error {
	d.input.Lock()
	defer d.input.Unlock()
	conn, root, err := d.begin(by)
	if err != nil {
		return err
	}
	cx, cy := d.clamp(x, y)
	if err := fake(conn, root, xproto.MotionNotify, 0, cx, cy); err != nil {
		return err
	}
	// Spaced like a hand rather than sent in one burst. xfwm4 answers a press
	// on a title-bar button by grabbing the pointer and waiting for the
	// release; a release already sent before that grab exists is never seen,
	// so the click only focused the window and the agent's second one closed
	// it. Every agent that tried it learned "click twice". The pause after the
	// motion is for the same race with Enter and hover, which GTK widgets
	// process before they accept a press.
	time.Sleep(clickSettle)
	count = max(1, min(count, 3))
	for i := 0; i < count; i++ {
		if err := fake(conn, root, xproto.ButtonPress, button, cx, cy); err != nil {
			return err
		}
		time.Sleep(clickHold)
		if err := fake(conn, root, xproto.ButtonRelease, button, cx, cy); err != nil {
			return err
		}
		if i < count-1 {
			// Inside every toolkit's double-click time (xfwm4's is 250ms),
			// outside the time some of them take as a bounce.
			time.Sleep(clickGap)
		}
	}
	kind := "click"
	if count == 2 {
		kind = "double_click"
	}
	if button == ButtonRight {
		kind = "right_click"
	}
	d.record(Action{By: by, Kind: kind, X: int(cx), Y: int(cy)})
	return nil
}

// Press and Release hold a button down and let it go, for a person dragging
// on the panel's view; an agent uses Drag.
func (d *Desktop) Press(by Source, x, y int, button byte, down bool) error {
	d.input.Lock()
	defer d.input.Unlock()
	conn, root, err := d.begin(by)
	if err != nil {
		return err
	}
	cx, cy := d.clamp(x, y)
	if err := fake(conn, root, xproto.MotionNotify, 0, cx, cy); err != nil {
		return err
	}
	typ := byte(xproto.ButtonRelease)
	if down {
		typ = xproto.ButtonPress
	}
	return fake(conn, root, typ, button, cx, cy)
}

// Drag presses at one point, moves to another in steps, and releases there.
func (d *Desktop) Drag(by Source, x1, y1, x2, y2 int) error {
	d.input.Lock()
	defer d.input.Unlock()
	conn, root, err := d.begin(by)
	if err != nil {
		return err
	}
	ax, ay := d.clamp(x1, y1)
	bx, by2 := d.clamp(x2, y2)
	if err := fake(conn, root, xproto.MotionNotify, 0, ax, ay); err != nil {
		return err
	}
	time.Sleep(clickSettle) // see Click
	if err := fake(conn, root, xproto.ButtonPress, ButtonLeft, ax, ay); err != nil {
		return err
	}
	time.Sleep(clickHold)
	// In steps rather than one jump: a toolkit that starts a drag only after
	// the pointer has moved some pixels with the button down never sees one
	// from a single motion event.
	const steps = 12
	for i := 1; i <= steps; i++ {
		x := int(ax) + (int(bx)-int(ax))*i/steps
		y := int(ay) + (int(by2)-int(ay))*i/steps
		if err := fake(conn, root, xproto.MotionNotify, 0, int16(x), int16(y)); err != nil {
			return err
		}
		time.Sleep(15 * time.Millisecond)
	}
	if err := fake(conn, root, xproto.ButtonRelease, ButtonLeft, bx, by2); err != nil {
		return err
	}
	d.record(Action{By: by, Kind: "drag", X: int(bx), Y: int(by2)})
	return nil
}

// Scroll turns the wheel at x, y: dy > 0 is down, dx > 0 is right, one unit
// a notch.
func (d *Desktop) Scroll(by Source, x, y, dx, dy int) error {
	d.input.Lock()
	defer d.input.Unlock()
	conn, root, err := d.begin(by)
	if err != nil {
		return err
	}
	cx, cy := d.clamp(x, y)
	if err := fake(conn, root, xproto.MotionNotify, 0, cx, cy); err != nil {
		return err
	}
	notch := func(button byte, n int) error {
		for i := 0; i < min(n, 30); i++ {
			if err := fake(conn, root, xproto.ButtonPress, button, cx, cy); err != nil {
				return err
			}
			if err := fake(conn, root, xproto.ButtonRelease, button, cx, cy); err != nil {
				return err
			}
		}
		return nil
	}
	var e error
	switch {
	case dy > 0:
		e = notch(wheelDown, dy)
	case dy < 0:
		e = notch(wheelUp, -dy)
	}
	if e == nil {
		switch {
		case dx > 0:
			e = notch(wheelRight, dx)
		case dx < 0:
			e = notch(wheelLeft, -dx)
		}
	}
	if e != nil {
		return e
	}
	d.record(Action{By: by, Kind: "scroll", X: int(cx), Y: int(cy)})
	return nil
}

// maxType bounds one `type`. A paragraph is fine; a file is a clipboard's job.
const maxType = 4000

// Type types text as keystrokes. Characters the keyboard map does not have --
// Chinese, emoji, anything outside the layout -- are typed through a spare
// keycode bound to that character for the one keystroke, which is xdotool's
// technique and works in every toolkit that reads keysyms.
func (d *Desktop) Type(by Source, text string) error {
	if len([]rune(text)) > maxType {
		return fmt.Errorf("desktop: %d characters is more than one type takes (%d); split it", len([]rune(text)), maxType)
	}
	d.input.Lock()
	defer d.input.Unlock()
	conn, root, err := d.begin(by)
	if err != nil {
		return err
	}
	for _, r := range text {
		if err := d.typeRune(conn, root, r); err != nil {
			return err
		}
	}
	summary := text
	if len([]rune(summary)) > 80 {
		summary = string([]rune(summary)[:80]) + "…"
	}
	d.record(Action{By: by, Kind: "type", Text: summary})
	return nil
}

func (d *Desktop) typeRune(conn *xgb.Conn, root xproto.Window, r rune) error {
	sym := runeKeysym(r)
	d.mu.Lock()
	km := d.keymap
	d.mu.Unlock()
	if code, shift, ok := km.lookup(sym); ok {
		return tapKey(conn, root, code, shift, km)
	}
	return d.typeViaSpare(conn, root, sym)
}

// typeViaSpare binds a spare keycode to sym, taps it, and unbinds it.
func (d *Desktop) typeViaSpare(conn *xgb.Conn, root xproto.Window, sym xproto.Keysym) error {
	d.mu.Lock()
	km := d.keymap
	d.mu.Unlock()
	if km.spare == 0 {
		return errors.New("desktop: the keyboard map has no unused keycode to type this character with")
	}
	per := int(km.perCode)
	syms := make([]xproto.Keysym, per)
	for i := range syms {
		syms[i] = sym
	}
	if err := xproto.ChangeKeyboardMappingChecked(conn, 1, km.spare, byte(per), syms).Check(); err != nil {
		return err
	}
	// Applications re-read the map on MappingNotify; a round trip makes sure
	// the server has applied it before the key arrives, and the pause gives
	// clients a chance to have read the notification.
	_, _ = xproto.GetInputFocus(conn).Reply()
	time.Sleep(12 * time.Millisecond)
	err := tapKey(conn, root, km.spare, false, km)
	_, _ = xproto.GetInputFocus(conn).Reply()
	time.Sleep(12 * time.Millisecond)
	blank := make([]xproto.Keysym, per)
	if uerr := xproto.ChangeKeyboardMappingChecked(conn, 1, km.spare, byte(per), blank).Check(); uerr != nil && err == nil {
		err = uerr
	}
	return err
}

// Key presses a combination such as "ctrl+l", "Return" or "ctrl+shift+t":
// modifiers held, the last key tapped, modifiers released in reverse.
func (d *Desktop) Key(by Source, combo string) error {
	keys, err := parseCombo(combo)
	if err != nil {
		return err
	}
	d.input.Lock()
	defer d.input.Unlock()
	conn, root, err := d.begin(by)
	if err != nil {
		return err
	}
	d.mu.Lock()
	km := d.keymap
	d.mu.Unlock()
	codes := make([]xproto.Keycode, 0, len(keys))
	for _, sym := range keys {
		code, _, ok := km.lookup(sym)
		if !ok {
			return fmt.Errorf("desktop: no key for %q on this keyboard map", combo)
		}
		codes = append(codes, code)
	}
	for _, c := range codes {
		if err := fake(conn, root, xproto.KeyPress, byte(c), 0, 0); err != nil {
			return err
		}
	}
	for i := len(codes) - 1; i >= 0; i-- {
		if err := fake(conn, root, xproto.KeyRelease, byte(codes[i]), 0, 0); err != nil {
			return err
		}
	}
	d.record(Action{By: by, Kind: "key", Text: combo})
	return nil
}

// KeyEvent sends one key down or up, for a person typing on the panel's
// view: the browser reports each key as it happens and holding one must hold
// it here too.
func (d *Desktop) KeyEvent(by Source, sym xproto.Keysym, down bool) error {
	d.input.Lock()
	defer d.input.Unlock()
	conn, root, err := d.begin(by)
	if err != nil {
		return err
	}
	d.mu.Lock()
	km := d.keymap
	d.mu.Unlock()
	code, _, ok := km.lookup(sym)
	if !ok {
		if !down {
			return nil
		}
		return d.typeViaSpare(conn, root, sym)
	}
	typ := byte(xproto.KeyRelease)
	if down {
		typ = xproto.KeyPress
	}
	return fake(conn, root, typ, byte(code), 0, 0)
}

// Watch subscribes to the live view; see stream.go.
func (d *Desktop) Watch(width int) (<-chan []byte, func()) {
	return d.frames.subscribe(width)
}

//go:build linux

package desktop

import (
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// animate opens a window and fills a moving bar across it at 30 frames a
// second, with a second connection of its own, until the returned function
// is called.
func animate(t *testing.T, disp string, w, h int) func() {
	conn, err := xgb.NewConnDisplay(disp)
	if err != nil {
		t.Fatal(err)
	}
	screen := xproto.Setup(conn).DefaultScreen(conn)
	win, _ := xproto.NewWindowId(conn)
	xproto.CreateWindow(conn, screen.RootDepth, win, screen.Root, 40, 40, uint16(w), uint16(h), 0,
		xproto.WindowClassInputOutput, screen.RootVisual, xproto.CwBackPixel, []uint32{screen.WhitePixel})
	xproto.MapWindow(conn, win)
	gc, _ := xproto.NewGcontextId(conn)
	xproto.CreateGC(conn, gc, xproto.Drawable(win), xproto.GcForeground, []uint32{0x3366cc})
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(33 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			x := int16((i * 12) % w)
			xproto.ChangeGC(conn, gc, xproto.GcForeground, []uint32{uint32(0x102030 + i*0x050301&0xffffff)})
			xproto.PolyFillRectangle(conn, xproto.Drawable(win), gc, []xproto.Rectangle{{X: x, Y: 0, Width: 60, Height: uint16(h)}})
			_, _ = xproto.GetInputFocus(conn).Reply()
		}
	}()
	return func() {
		close(done)
		time.Sleep(50 * time.Millisecond)
		xproto.DestroyWindow(conn, win)
		_, _ = xproto.GetInputFocus(conn).Reply()
		conn.Close()
	}
}

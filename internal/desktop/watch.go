package desktop

import (
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// Watching for change: which rectangles of the screen the server says were
// drawn on, so the stream captures those and nothing else.
//
// Damage on the root window is precise on a plain X desktop and useless under
// a compositing window manager. Measured under xfwm4's compositor (on by
// default in Xfce, so on the machine this was written for): every repaint
// reports the whole screen, three times, however small the change. A stream
// trusting that captured 1920x1080 for a 60-pixel bar moving across a small
// window -- 38 ms a frame, six frames a second.
//
// So under a compositor this does what compositors themselves do (xcompmgr's
// approach): a damage object on every top-level window, whose notifications
// carry the window's position, and the window's own appearing, disappearing,
// moving and resizing reported from SubstructureNotify on the root. Whether a
// compositor is running is whoever owns the _NET_WM_CM_S0 selection.

type watcher struct {
	conn *xgb.Conn
	root xproto.Window
	q    *damageQueue

	mu   sync.Mutex
	wins map[xproto.Window]rect
}

func watchDamage(conn *xgb.Conn, root xproto.Window, screen int) (*watcher, error) {
	if err := xfixes.Init(conn); err != nil {
		return nil, err
	}
	if _, err := xfixes.QueryVersion(conn, 4, 0).Reply(); err != nil {
		return nil, err
	}
	if err := damage.Init(conn); err != nil {
		return nil, err
	}
	if _, err := damage.QueryVersion(conn, 1, 1).Reply(); err != nil {
		return nil, err
	}
	w := &watcher{conn: conn, root: root, q: newDamageQueue(), wins: map[xproto.Window]rect{}}
	if !composited(conn, screen) {
		// Raw rectangles: every change reported, and nothing to acknowledge.
		// The other levels need a Subtract per notification to keep them
		// coming, a round trip each that this has no use for.
		id, err := damage.NewDamageId(conn)
		if err != nil {
			return nil, err
		}
		if err := damage.CreateChecked(conn, id, xproto.Drawable(root), damage.ReportLevelRawRectangles).Check(); err != nil {
			return nil, err
		}
		return w, nil
	}
	if err := xproto.ChangeWindowAttributesChecked(conn, root, xproto.CwEventMask,
		[]uint32{xproto.EventMaskSubstructureNotify}).Check(); err != nil {
		return nil, err
	}
	tree, err := xproto.QueryTree(conn, root).Reply()
	if err != nil {
		return nil, err
	}
	for _, win := range tree.Children {
		w.track(win)
	}
	return w, nil
}

// composited is whether a compositing manager is running on the screen.
func composited(conn *xgb.Conn, screen int) bool {
	name := "_NET_WM_CM_S" + string(rune('0'+screen%10))
	atom, err := xproto.InternAtom(conn, true, uint16(len(name)), name).Reply()
	if err != nil || atom.Atom == 0 {
		return false
	}
	owner, err := xproto.GetSelectionOwner(conn, atom.Atom).Reply()
	return err == nil && owner.Owner != 0
}

// track starts watching one top-level window, if it is on screen.
func (w *watcher) track(win xproto.Window) {
	attrs, err := xproto.GetWindowAttributes(w.conn, win).Reply()
	if err != nil || attrs.MapState != xproto.MapStateViewable || attrs.Class == xproto.WindowClassInputOnly {
		return
	}
	geo, err := xproto.GetGeometry(w.conn, xproto.Drawable(win)).Reply()
	if err != nil {
		return
	}
	id, err := damage.NewDamageId(w.conn)
	if err != nil {
		return
	}
	// A window can be gone by the time the request arrives; the error is
	// the ordinary outcome then, and there is nothing to watch.
	if damage.CreateChecked(w.conn, id, xproto.Drawable(win), damage.ReportLevelRawRectangles).Check() != nil {
		return
	}
	r := rect{int(geo.X), int(geo.Y), int(geo.Width) + 2*int(geo.BorderWidth), int(geo.Height) + 2*int(geo.BorderWidth)}
	w.mu.Lock()
	w.wins[win] = r
	w.mu.Unlock()
	w.q.add(r.x, r.y, r.w, r.h)
}

func (w *watcher) forget(win xproto.Window) {
	w.mu.Lock()
	r, ok := w.wins[win]
	delete(w.wins, win)
	w.mu.Unlock()
	if ok {
		w.q.add(r.x, r.y, r.w, r.h)
	}
}

// event handles one event from the connection: damage, or a top-level
// window changing.
func (w *watcher) event(ev xgb.Event) {
	switch e := ev.(type) {
	case damage.NotifyEvent:
		// Area is in the drawable's coordinates and Geometry is where the
		// drawable is: for a top-level window that is the screen, and for
		// the root it is the origin.
		w.q.add(int(e.Geometry.X)+int(e.Area.X), int(e.Geometry.Y)+int(e.Area.Y), int(e.Area.Width), int(e.Area.Height))
	case xproto.MapNotifyEvent:
		if e.Event == w.root {
			go w.track(e.Window)
		}
	case xproto.UnmapNotifyEvent:
		if e.Event == w.root {
			w.forget(e.Window)
		}
	case xproto.DestroyNotifyEvent:
		if e.Event == w.root {
			w.forget(e.Window)
		}
	case xproto.ConfigureNotifyEvent:
		if e.Event != w.root {
			return
		}
		w.mu.Lock()
		old, ok := w.wins[e.Window]
		nr := rect{int(e.X), int(e.Y), int(e.Width) + 2*int(e.BorderWidth), int(e.Height) + 2*int(e.BorderWidth)}
		if ok {
			w.wins[e.Window] = nr
		}
		w.mu.Unlock()
		if ok {
			// Where it was is uncovered, where it is now is drawn: both.
			w.q.add(old.x, old.y, old.w, old.h)
			w.q.add(nr.x, nr.y, nr.w, nr.h)
		}
	}
}

// drainEvents reads the connection's events for as long as it is open.
// Somebody has to: xgb queues them, and a full queue stops the connection --
// every capture and every click with it.
func drainEvents(conn *xgb.Conn, w *watcher) {
	for {
		ev, xerr := conn.WaitForEvent()
		if ev == nil && xerr == nil {
			return
		}
		if w != nil && ev != nil {
			w.event(ev)
		}
	}
}

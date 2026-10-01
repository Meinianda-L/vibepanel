//go:build linux

package desktop

import (
	"errors"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"
	"golang.org/x/sys/unix"
)

// shmGrabber captures through MIT-SHM: the X server writes the pixels into a
// System V shared memory segment this process has mapped, instead of sending
// them down the socket.
//
// Measured on the machine this was written for (a Core 2 Duo P8600), a
// 1920x1080 GetImage over the socket took 95 ms and allocated 25 MB every
// time; that was most of a core at ten frames a second before a single byte
// was encoded. Through shared memory the copy happens once, inside the
// server, into memory that is reused.
type shmGrabber struct {
	conn *xgb.Conn
	root xproto.Window
	seg  shm.Seg
	mem  []byte
}

func newSHMGrabber(conn *xgb.Conn, root xproto.Window, w, h int) (*shmGrabber, error) {
	if err := shm.Init(conn); err != nil {
		return nil, err
	}
	if _, err := shm.QueryVersion(conn).Reply(); err != nil {
		return nil, err
	}
	// Twice the screen: the screen buffer, and scratch for rectangles
	// narrower than it.
	size := 2 * w * h * 4
	id, err := unix.SysvShmGet(unix.IPC_PRIVATE, size, unix.IPC_CREAT|0o600)
	if err != nil {
		return nil, err
	}
	mem, err := unix.SysvShmAttach(id, 0, 0)
	if err != nil {
		_, _ = unix.SysvShmCtl(id, unix.IPC_RMID, nil)
		return nil, err
	}
	seg, err := shm.NewSegId(conn)
	if err != nil {
		_ = unix.SysvShmDetach(mem)
		_, _ = unix.SysvShmCtl(id, unix.IPC_RMID, nil)
		return nil, err
	}
	// The server attaches before the segment is marked for removal, and the
	// mark is made at once after: from then on the kernel frees it when both
	// sides detach, so a panel that is killed leaves no segment behind.
	attachErr := shm.AttachChecked(conn, seg, uint32(id), false).Check()
	_, _ = unix.SysvShmCtl(id, unix.IPC_RMID, nil)
	if attachErr != nil {
		_ = unix.SysvShmDetach(mem)
		// A display on another machine cannot see this machine's memory;
		// that is the ordinary reason, and the socket path still works.
		return nil, attachErr
	}
	return &shmGrabber{conn: conn, root: root, seg: seg, mem: mem}, nil
}

// grab brings a rectangle of the screen into the front of the segment, which
// is the screen buffer itself (see Desktop.cur).
//
// The server writes a rectangle as consecutive rows of its own width, so a
// rectangle as wide as the screen lands in screen layout and is written
// straight to where it belongs, with no copy on this side. Anything narrower
// goes to the scratch half of the segment and its rows are copied across.
func (g *shmGrabber) grab(x, y, w, h int, stride int) error {
	if w <= 0 || h <= 0 {
		return nil
	}
	screen := len(g.mem) / 2
	direct := x == 0 && w*4 == stride
	offset := y * stride
	if !direct {
		offset = screen
		if w*h*4 > screen {
			return errors.New("desktop: rectangle larger than the screen")
		}
	}
	if _, err := shm.GetImage(g.conn, xproto.Drawable(g.root), int16(x), int16(y), uint16(w), uint16(h),
		0xffffffff, xproto.ImageFormatZPixmap, g.seg, uint32(offset)).Reply(); err != nil {
		return err
	}
	if direct {
		return nil
	}
	row := w * 4
	for r := 0; r < h; r++ {
		copy(g.mem[(y+r)*stride+x*4:(y+r)*stride+x*4+row], g.mem[screen+r*row:screen+(r+1)*row])
	}
	return nil
}

// screen is the front half of the segment: the screen buffer.
func (g *shmGrabber) screen() []byte { return g.mem[:len(g.mem)/2] }

func (g *shmGrabber) close() {
	_ = shm.DetachChecked(g.conn, g.seg).Check()
	_ = unix.SysvShmDetach(g.mem)
}

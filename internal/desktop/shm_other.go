//go:build !linux

package desktop

import (
	"errors"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// Outside Linux there is no System V shared memory to hand an X server
// through x/sys, so capture always goes through the socket.
type shmGrabber struct{}

func newSHMGrabber(*xgb.Conn, xproto.Window, int, int) (*shmGrabber, error) {
	return nil, errors.New("desktop: MIT-SHM capture is Linux only")
}

func (g *shmGrabber) grab(int, int, int, int, int) error {
	return errors.New("desktop: MIT-SHM capture is Linux only")
}

func (g *shmGrabber) close() {}

func (g *shmGrabber) screen() []byte { return nil }

//go:build linux

package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestShowDamage prints the damage rectangles the server reports while a
// small animation runs, to see what a compositing window manager does to
// them. Diagnostic only.
func TestShowDamage(t *testing.T) {
	disp := os.Getenv("VIBEPANEL_DESKTOP_TEST")
	if disp == "" {
		t.Skip("VIBEPANEL_DESKTOP_TEST is not set")
	}
	d := Open(disp)
	if _, _, err := d.Size(); err != nil {
		t.Fatal(err)
	}
	anim := exec.Command(os.Args[0], "-test.run", "TestAnimateHelper")
	anim.Env = append(os.Environ(), "VP_ANIMATE=480x320")
	_ = anim.Start()
	defer func() { _ = anim.Process.Kill(); _ = anim.Wait() }()
	time.Sleep(time.Second)
	d.mu.Lock()
	q := d.damaged
	d.mu.Unlock()
	q.take(nil)
	for i := 0; i < 4; i++ {
		time.Sleep(100 * time.Millisecond)
		rs, all := q.take(nil)
		fmt.Printf("batch %d: all=%v %v\n", i, all, rs)
	}
}

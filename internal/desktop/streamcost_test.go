//go:build linux

package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestStreamCost watches a real display for a while and reports what it cost
// this process: CPU time, messages and bytes. Only where
// VIBEPANEL_DESKTOP_TEST names a display; it does not touch the pointer or
// the keyboard.
func TestStreamCost(t *testing.T) {
	disp := os.Getenv("VIBEPANEL_DESKTOP_TEST")
	if disp == "" {
		t.Skip("VIBEPANEL_DESKTOP_TEST is not set")
	}
	d := Open(disp)
	if _, _, err := d.Size(); err != nil {
		t.Fatal(err)
	}
	cpu := func() time.Duration {
		var ru syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
		return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	ch, stop := d.Watch(960)
	defer stop()
	// The keyframe a new viewer gets, measured on its own.
	c0 := cpu()
	t0 := time.Now()
	first := <-ch
	t.Logf("first frame: %d KB after %v, %v CPU", len(first)/1024, time.Since(t0).Round(time.Millisecond), (cpu() - c0).Round(time.Millisecond))

	const watch = 15 * time.Second
	c1 := cpu()
	end := time.After(watch)
	msgs, bytes := 0, 0
	for {
		select {
		case m := <-ch:
			msgs++
			bytes += len(m)
		case <-end:
			used := cpu() - c1
			t.Logf("watching %v: %d messages, %d KB, %v CPU = %.1f%% of one core",
				watch, msgs, bytes/1024, used.Round(time.Millisecond), 100*used.Seconds()/watch.Seconds())
			return
		}
	}
}

// TestStreamCostAnimating runs an animation in a window of its own -- a small
// one, then one nearly the size of the screen -- in a separate process, so
// the CPU reported is the stream's alone, and reports what streaming it cost
// and where the time went. The window is destroyed after; the pointer and
// keyboard are not touched.
func TestStreamCostAnimating(t *testing.T) {
	disp := os.Getenv("VIBEPANEL_DESKTOP_TEST")
	if disp == "" {
		t.Skip("VIBEPANEL_DESKTOP_TEST is not set")
	}
	d := Open(disp)
	if _, _, err := d.Size(); err != nil {
		t.Fatal(err)
	}
	cpu := func() time.Duration {
		var ru syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
		return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	for _, size := range []string{"480x320", "1600x900"} {
		anim := exec.Command(os.Args[0], "-test.run", "TestAnimateHelper")
		anim.Env = append(os.Environ(), "VP_ANIMATE="+size)
		if err := anim.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		ch, stop := d.Watch(960)
		<-ch
		before := d.Stats()
		const watch = 10 * time.Second
		c1 := cpu()
		end := time.After(watch)
		msgs, bytes := 0, 0
	loop:
		for {
			select {
			case m := <-ch:
				msgs++
				bytes += len(m)
			case <-end:
				break loop
			}
		}
		used := cpu() - c1
		stop()
		_ = anim.Process.Kill()
		_ = anim.Wait()
		st := d.Stats()
		f := max(1, st.Frames-before.Frames)
		t.Logf("%s animation: %.1f frames/s, %d KB/s, %.1f%% of one core; per frame: %d damage rects, %d whole-screen, %d px captured, %d tiles changed, grab %v diff %v encode %v",
			size, float64(msgs)/watch.Seconds(), bytes/1024/int(watch.Seconds()), 100*used.Seconds()/watch.Seconds(),
			(st.DamageRects-before.DamageRects)/f, st.WholeScreen-before.WholeScreen, (st.Captured-before.Captured)/f,
			(st.Changed-before.Changed)/f, ((st.Grab - before.Grab) / time.Duration(f)).Round(100*time.Microsecond),
			((st.Diff - before.Diff) / time.Duration(f)).Round(100*time.Microsecond), ((st.Encode - before.Encode) / time.Duration(f)).Round(100*time.Microsecond))
		time.Sleep(500 * time.Millisecond)
	}
}

// TestAnimateHelper is the animation, run as its own process by the test
// above.
func TestAnimateHelper(t *testing.T) {
	size := os.Getenv("VP_ANIMATE")
	if size == "" {
		t.Skip("run by TestStreamCostAnimating")
	}
	var w, h int
	fmt.Sscanf(size, "%dx%d", &w, &h)
	stop := animate(t, os.Getenv("VIBEPANEL_DESKTOP_TEST"), w, h)
	defer stop()
	time.Sleep(time.Minute)
}

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// `vibepanel desktop <action> ...` is the desktop for an agent that has a
// shell but no MCP -- pi, deliberately, and anything else that can run a
// command and read an image file. The same two panel routes as
// `desktop-mcp`, so Stop and the audit log apply exactly the same.
//
// Every action prints one line saying what it did and where the screenshot
// taken after it was saved; the agent reads that file to see the screen.
func cmdDesktop(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(desktopUsage)
		return nil
	}
	c, err := desktopClientFromFile("")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	action, rest := args[0], args[1:]
	nums := func(n int) ([]int, error) {
		if len(rest) < n {
			return nil, fmt.Errorf("%s needs %d numbers", action, n)
		}
		out := make([]int, n)
		for i := range out {
			v, err := strconv.Atoi(rest[i])
			if err != nil {
				return nil, fmt.Errorf("%s: %q is not a number", action, rest[i])
			}
			out[i] = v
		}
		return out, nil
	}
	act := map[string]any{"action": action}
	switch action {
	case "screenshot", "shot":
		var s desktopShot
		if err := c.do(ctx, http.MethodGet, "/screenshot", nil, &s); err != nil {
			return err
		}
		return printShot("", s)
	case "click", "double_click", "triple_click", "right_click", "middle_click", "move":
		v, err := nums(2)
		if err != nil {
			return err
		}
		act["x"], act["y"] = v[0], v[1]
	case "drag":
		v, err := nums(4)
		if err != nil {
			return err
		}
		act["x"], act["y"], act["x2"], act["y2"] = v[0], v[1], v[2], v[3]
	case "scroll":
		v, err := nums(3)
		if err != nil {
			return err
		}
		act["x"], act["y"], act["dy"] = v[0], v[1], v[2]
	case "type":
		if len(rest) == 0 {
			return errors.New("type needs the text")
		}
		act["text"] = strings.Join(rest, " ")
	case "key":
		if len(rest) != 1 {
			return errors.New("key needs one key or combination, e.g. ctrl+l")
		}
		act["key"] = rest[0]
	case "wait":
		secs := 1.0
		if len(rest) > 0 {
			if f, err := strconv.ParseFloat(rest[0], 64); err == nil {
				secs = f
			}
		}
		act["seconds"] = secs
	default:
		return fmt.Errorf("no desktop action %q\n\n%s", action, desktopUsage)
	}
	var res struct {
		OK         bool         `json:"ok"`
		Screenshot *desktopShot `json:"screenshot"`
	}
	if err := c.do(ctx, http.MethodPost, "/act", act, &res); err != nil {
		return err
	}
	if res.Screenshot == nil {
		fmt.Println("done")
		return nil
	}
	return printShot("done. ", *res.Screenshot)
}

const desktopUsage = `usage: vibepanel desktop <action> [arguments]

Operate the desktop the panel shows (--desktop). Coordinates are pixels of
the screenshots this prints, not of the physical screen. Every action saves a
screenshot taken just after it and prints its path; read that file to see the
screen.

  screenshot                 save a screenshot and print its path
  click X Y                  left-click
  double_click X Y
  triple_click X Y           select a line of text
  right_click X Y            context menu
  move X Y                   hover without clicking
  drag X Y X2 Y2             press at X Y, release at X2 Y2
  scroll X Y DY              DY notches down (negative is up)
  type TEXT...               type text, any language; click the field first
  key KEY                    Return, Escape, Tab, ctrl+l, ctrl+shift+t, alt+F4 ...
  wait [SECONDS]             wait (at most 10), then screenshot

If an action is refused because the person at the panel stopped you or is
using the screen, stop and tell them rather than retrying.
`

// desktopClientFromFile reads the panel's desktop-agent.json.
func desktopClientFromFile(path string) (*desktopClient, error) {
	if path == "" {
		path = defaultDesktopAgentFile()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("vibepanel desktop: %w (is the panel running with --desktop?)", err)
	}
	var agent struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &agent); err != nil || agent.URL == "" || agent.Token == "" {
		return nil, fmt.Errorf("vibepanel desktop: %s is not a desktop agent file", path)
	}
	return newDesktopClient(agent.URL, agent.Token)
}

// printShot saves the screenshot where the agent can read it and says so.
// One file, overwritten: the agent only ever needs the latest, and a
// directory of them is a disk filling a frame at a time.
func printShot(prefix string, s desktopShot) error {
	b, err := base64.StdEncoding.DecodeString(s.Image)
	if err != nil {
		return err
	}
	dir := filepath.Join(os.TempDir(), "vibepanel-desktop-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "screen.jpg")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	fmt.Printf("%sScreenshot %dx%d saved to %s (the screen is %dx%d; use the screenshot's coordinates). Pointer at %d,%d.\n",
		prefix, s.Width, s.Height, path, s.ScreenWidth, s.ScreenHeight, s.PointerX, s.PointerY)
	if s.Stopped {
		fmt.Println("The person at the panel has stopped you: do not act until they resume.")
	}
	return nil
}

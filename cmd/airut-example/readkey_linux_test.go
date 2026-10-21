//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal, and returns its controlling side and
// the terminal a program reads.
func openPTY(t *testing.T) (ptm, pts *os.File) {
	t.Helper()
	ptm, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pseudo-terminals:", err)
	}
	t.Cleanup(func() { ptm.Close() })
	fd := int(ptm.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	pts, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pts.Close() })
	go func() { _, _ = io.Copy(io.Discard, ptm) }()
	return ptm, pts
}

// TestReadKeyLeavesTheTerminalAsItFoundIt reads a key typed on a
// terminal, and gives up on one that never comes, and either way leaves
// the terminal's modes, echo among them, as they were.
func TestReadKeyLeavesTheTerminalAsItFoundIt(t *testing.T) {
	ptm, pts := openPTY(t)
	fd := int(pts.Fd())
	want, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if want.Lflag&unix.ECHO == 0 {
		t.Fatal("the new terminal does not echo")
	}
	same := func(when string) {
		t.Helper()
		got, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		if *got != *want {
			t.Errorf("%s the terminal's modes are %+v, want %+v", when, *got, *want)
		}
	}

	// Type the key once readKey has the terminal in raw mode, as a person
	// does after the prompt.
	bounded, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	type read struct {
		key string
		err error
	}
	got := make(chan read, 1)
	go func() {
		key, err := readKey(bounded, pts, pts, "Recovery key: ")
		got <- read{key, err}
	}()
	for {
		st, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		if st.Lflag&unix.ICANON == 0 {
			break
		}
		select {
		case r := <-got:
			t.Fatalf("readKey returned before raw mode: %q, %v", r.key, r.err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err := ptm.Write([]byte("EsTc abcd\r")); err != nil {
		t.Fatal(err)
	}
	if r := <-got; r.key != "EsTc abcd" || r.err != nil {
		t.Errorf("readKey = %q, %v", r.key, r.err)
	}
	same("after a key")

	// The read readKey leaves blocked must not change the terminal later.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readKey(ctx, pts, pts, "Recovery key: "); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled read = %v", err)
	}
	same("after a cancelled read")
	time.Sleep(100 * time.Millisecond)
	same("a moment after a cancelled read")
}

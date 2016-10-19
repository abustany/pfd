// +build linux

package main

import (
	"errors"
	"syscall"
	"unsafe"
)

type winsize struct {
	ws_row    uint16
	ws_col    uint16
	ws_xpixel uint16 // unused
	ws_ypixel uint16 // unused
}

func GetTerminalWidth() (uint, error) {
	w := &winsize{}

	res, _, _ := syscall.Syscall(syscall.SYS_IOCTL, uintptr(syscall.Stdout), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(w)))

	if int(res) != 0 {
		return 0, errors.New("TIOCGWINSZ syscall failed")
	}

	return uint(w.ws_col), nil
}

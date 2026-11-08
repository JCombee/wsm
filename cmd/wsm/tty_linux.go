package main

import (
	"syscall"
	"unsafe"
)

// termState is a saved terminal mode.
type termState struct{ t syscall.Termios }

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// isTerminal reports whether fd is a terminal.
func isTerminal(fd int) bool {
	var t syscall.Termios
	return ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t)) == nil
}

// makeRaw puts the terminal in raw mode, like cfmakeraw(3), and returns
// the previous state.
func makeRaw(fd int) (*termState, error) {
	var old syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&old)); err != nil {
		return nil, err
	}
	t := old
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, syscall.TCSETS, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return &termState{old}, nil
}

// restore puts the terminal back in a saved mode.
func restore(fd int, s *termState) error {
	return ioctl(fd, syscall.TCSETS, unsafe.Pointer(&s.t))
}

// termSize returns the rows and columns of the terminal on fd.
func termSize(fd int) (rows, cols int, ok bool) {
	var ws struct{ Row, Col, X, Y uint16 }
	if err := ioctl(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil || ws.Row == 0 || ws.Col == 0 {
		return 0, 0, false
	}
	return int(ws.Row), int(ws.Col), true
}

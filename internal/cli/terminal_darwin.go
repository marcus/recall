//go:build darwin

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

func isTerminal(f *os.File) bool {
	var state syscall.Termios
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TIOCGETA),
		uintptr(unsafe.Pointer(&state)),
		0,
		0,
		0,
	)
	return errno == 0
}

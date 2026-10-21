// Package sysproc holds OS-specific process attributes for helper commands.
package sysproc

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// NoWindow keeps a console program (git) from opening a window of its own
// when the caller has no console, e.g. the detached hub.
func NoWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW, HideWindow: true}
}

//go:build !windows

// Package sysproc holds OS-specific process attributes for helper commands.
package sysproc

import "syscall"

// NoWindow is a no-op outside Windows.
func NoWindow() *syscall.SysProcAttr { return nil }

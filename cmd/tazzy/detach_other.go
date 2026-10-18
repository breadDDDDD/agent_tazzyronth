//go:build !windows

package main

import "syscall"

// detached makes the hub outlive the shim and its terminal session.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

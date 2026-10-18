package main

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detached makes the hub outlive the shim and its console.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

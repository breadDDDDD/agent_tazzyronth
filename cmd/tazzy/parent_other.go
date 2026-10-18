//go:build !windows

package main

import (
	"os"
	"time"
)

// parentGone is closed when the process that started this one exits (we
// get reparented), so a shim never outlives its host.
func parentGone() <-chan struct{} {
	done := make(chan struct{})
	ppid := os.Getppid()
	go func() {
		for os.Getppid() == ppid {
			time.Sleep(time.Second)
		}
		close(done)
	}()
	return done
}

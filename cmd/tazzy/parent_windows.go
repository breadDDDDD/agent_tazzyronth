package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// parentGone is closed when the process that started this one exits, so a
// shim never outlives its host (Windows doesn't kill children with their
// parent, and a dead host can leave our stdin open). A parent created after
// us is a reused PID: the real one is already gone.
func parentGone() <-chan struct{} {
	done := make(chan struct{})
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(os.Getppid()))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER { // no such process
			close(done)
		}
		return done // can't watch it (access denied): never fire
	}
	if started(h) > started(windows.CurrentProcess()) {
		windows.CloseHandle(h)
		close(done)
		return done
	}
	go func() {
		defer windows.CloseHandle(h)
		windows.WaitForSingleObject(h, windows.INFINITE)
		close(done)
	}()
	return done
}

func started(h windows.Handle) int64 {
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0
	}
	return c.Nanoseconds()
}

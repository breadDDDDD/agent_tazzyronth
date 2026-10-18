package supervisor

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
}

// tree is a Job Object holding the worker and everything it starts. The job
// has KILL_ON_JOB_CLOSE, and only this process holds its handle, so the tree
// dies when the worker exits (close), on kill, and when the hub dies.
// Children the worker starts before it is assigned escape the job; the window
// is a few microseconds after CreateProcess.
type tree struct{ job windows.Handle }

func newTree(p *os.Process) (tree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return tree{}, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return tree{}, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return tree{}, err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return tree{}, err
	}
	return tree{job}, nil
}

func (t tree) kill()  { windows.TerminateJobObject(t.job, 1) }
func (t tree) close() { windows.CloseHandle(t.job) }

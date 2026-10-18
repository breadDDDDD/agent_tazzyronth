//go:build !windows

package supervisor

import (
	"os"
	"syscall"
)

func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// tree is the worker's process group. Unlike a Job Object it does not die with
// the hub by itself: the hub kills it on a clean shutdown.
type tree struct{ pgid int }

func newTree(p *os.Process) (tree, error) { return tree{p.Pid}, nil }

func (t tree) kill()  { syscall.Kill(-t.pgid, syscall.SIGKILL) }
func (t tree) close() { t.kill() } // leftover children go with the worker

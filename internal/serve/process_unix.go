//go:build !windows

package serve

import (
	"errors"
	"os"
	"syscall"
)

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	// EPERM means the process exists but belongs to another user
	return err == nil || errors.Is(err, syscall.EPERM)
}

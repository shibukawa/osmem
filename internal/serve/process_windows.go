//go:build windows

package serve

import "os"

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// on Windows FindProcess succeeds only for running processes
	_ = p.Release()
	return true
}

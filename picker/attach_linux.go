package main

import "syscall"

// Pdeathsig lets the launcher's own TERM trap roll back when the picker dies
// uncatchably (SIGKILL).
func attachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGTERM}
}

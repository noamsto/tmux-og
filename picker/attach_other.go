//go:build !linux

package main

import "syscall"

func attachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

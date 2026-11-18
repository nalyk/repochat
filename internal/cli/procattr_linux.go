//go:build linux

package cli

import "syscall"

// helperProcAttr starts tunnel-client in its own process group, so signals
// reach it only through repochat, and asks the kernel to terminate it if
// repochat itself dies.
func helperProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

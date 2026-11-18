//go:build darwin

package cli

import "syscall"

// helperProcAttr starts tunnel-client in its own process group, so signals
// reach it only through repochat.
func helperProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

//go:build windows

package update

import "syscall"

// procAttr keeps the package buildable on a Windows host, where the bridge is
// only checked and cross-compiled: the installer path itself is Linux-only and
// refuses to run elsewhere.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

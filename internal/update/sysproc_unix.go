//go:build !windows

package update

import "syscall"

// procAttr detaches the installer from the bridge's process group: the script
// restarts the service that started it, so it must survive the restart.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

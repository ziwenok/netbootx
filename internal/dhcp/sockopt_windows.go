//go:build windows

package dhcp

import "syscall"

// setSockoptBroadcast 在 Windows 上开启 SO_BROADCAST。
func setSockoptBroadcast(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
}

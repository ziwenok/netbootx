//go:build !windows

package dhcp

import "syscall"

// setSockoptBroadcast 在 Unix 上开启 SO_BROADCAST。
func setSockoptBroadcast(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
}

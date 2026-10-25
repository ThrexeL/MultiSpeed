//go:build windows

package transport

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// interfaceDescription returns the Windows adapter description (for example
// "Remote NDIS Compatible Device") for the interface whose friendly name is
// name. It returns "" when the adapter cannot be found.
func interfaceDescription(name string) string {
	size := uint32(16 * 1024)
	for attempt := 0; attempt < 4; attempt++ {
		buf := make([]byte, size)
		addrs := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(
			windows.AF_UNSPEC,
			windows.GAA_FLAG_SKIP_ANYCAST|windows.GAA_FLAG_SKIP_MULTICAST,
			0,
			addrs,
			&size,
		)
		if err != nil {
			if err == windows.ERROR_BUFFER_OVERFLOW {
				continue
			}
			return ""
		}
		for a := addrs; a != nil; a = a.Next {
			if a.FriendlyName == nil {
				continue
			}
			if windows.UTF16PtrToString(a.FriendlyName) != name {
				continue
			}
			if a.Description == nil {
				return ""
			}
			return windows.UTF16PtrToString(a.Description)
		}
		return ""
	}
	return ""
}

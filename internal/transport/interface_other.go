//go:build !windows

package transport

// interfaceDescription has no portable equivalent outside Windows, so link
// kinds are inferred from the interface name alone.
func interfaceDescription(string) string { return "" }

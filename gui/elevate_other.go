//go:build !windows

package main

import "errors"

// isElevated is only meaningful on Windows; Linux asks for root when needed.
func isElevated() bool { return true }

func relaunchElevated() error {
	return errors.New("restarting as administrator is only supported on Windows")
}

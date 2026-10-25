//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// platformChecks reports the Windows prerequisites for a real tunnel: an
// elevated token for TUN creation and route changes, and the Wintun driver DLL.
func platformChecks() []preflightCheck {
	elevated := windows.GetCurrentProcessToken().IsElevated()
	admin := preflightCheck{Name: "administrator", OK: elevated, Detail: "yes"}
	if !elevated {
		admin.Detail = "no"
		admin.Hint = "click Start, type powershell, right-click Windows PowerShell and choose Run as administrator"
	}

	wintun := preflightCheck{Name: "wintun.dll"}
	if path, err := findWintun(); err != nil {
		wintun.Detail = err.Error()
		wintun.Hint = "download the official build from https://www.wintun.net/ and copy bin/amd64/wintun.dll next to multispeed-cli.exe"
	} else {
		wintun.OK = true
		wintun.Detail = path
	}
	return []preflightCheck{admin, wintun}
}

// findWintun looks next to the executable and in the current directory, the two
// places an operator is expected to install it. It deliberately ignores PATH so
// a DLL belonging to another application is never picked up by accident.
func findWintun() (string, error) {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "wintun.dll")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("not found next to the executable or in the current directory")
}

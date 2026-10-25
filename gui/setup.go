package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/multispeed-app/multispeed/internal/config"
	"github.com/multispeed-app/multispeed/internal/transport"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	wintunPage       = "https://www.wintun.net/"
	wintunZipURL     = "https://www.wintun.net/builds/wintun-0.14.1.zip"
	wintunDLLEntry   = "wintun/bin/amd64/wintun.dll"
	wintunLicense    = "wintun/LICENSE.txt"
	maxWintunZipSize = 16 << 20
)

// AdapterView is one physical adapter the user can add to the tunnel.
type AdapterView struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
	Kind      string `json:"kind"`
	Weight    int    `json:"weight"`
	MTU       int    `json:"mtu"`
}

// TUNStatus reports whether this machine can create the tunnel device.
type TUNStatus struct {
	Platform    string `json:"platform"`
	Required    bool   `json:"required"`
	Ready       bool   `json:"ready"`
	Location    string `json:"location"`
	DownloadURL string `json:"download_url"`
	Detail      string `json:"detail,omitempty"`
}

// SetupState is everything the first-run wizard needs to render.
type SetupState struct {
	Version     string        `json:"version"`
	Platform    string        `json:"platform"`
	Elevated    bool          `json:"elevated"`
	NeedsSetup  bool          `json:"needs_setup"`
	HasProfile  bool          `json:"has_profile"`
	Profile     Profile       `json:"profile"`
	ProfilePath string        `json:"profile_path"`
	Adapters    []AdapterView `json:"adapters"`
	TUN         TUNStatus     `json:"tun"`
	Warning     string        `json:"warning,omitempty"`
}

// JoinPreview is the result of validating a pasted join code.
type JoinPreview struct {
	Server     string        `json:"server"`
	ClientName string        `json:"client_name"`
	Adapters   []AdapterView `json:"adapters"`
}

// GetSetup returns the wizard state: detected adapters, runtime readiness, and
// the current profile summary.
func (a *App) GetSetup() SetupState {
	a.mu.Lock()
	profile := a.profile
	hasProfile := a.profilePath != ""
	a.mu.Unlock()

	state := SetupState{
		Version:     appVersion,
		Platform:    runtime.GOOS,
		Elevated:    isElevated(),
		NeedsSetup:  !hasProfile,
		HasProfile:  hasProfile,
		Profile:     profile,
		ProfilePath: defaultProfileSavePath(),
		TUN:         wintunStatus(),
	}
	adapters, err := discoverAdapters()
	if err != nil {
		state.Warning = err.Error()
	}
	state.Adapters = adapters
	return state
}

// PreviewJoin validates a join code and returns the relay and local adapters.
// Nothing is written until SaveSetup is called.
func (a *App) PreviewJoin(code string) (JoinPreview, error) {
	join, err := config.DecodeJoinCode(code)
	if err != nil {
		return JoinPreview{}, err
	}
	adapters, err := discoverAdapters()
	if err != nil {
		return JoinPreview{}, err
	}
	return JoinPreview{
		Server:     join.Server,
		ClientName: join.Name,
		Adapters:   adapters,
	}, nil
}

// SaveSetup turns a join code and the selected adapters into a client profile.
// The relay stays reachable on every selected adapter, and the tunnel balances
// traffic over all of them.
func (a *App) SaveSetup(code string, interfaces []string) (Profile, error) {
	join, err := config.DecodeJoinCode(code)
	if err != nil {
		return Profile{}, err
	}
	clientCfg, err := join.Client()
	if err != nil {
		return Profile{}, err
	}
	adapters, err := discoverAdapters()
	if err != nil {
		return Profile{}, err
	}
	byName := make(map[string]AdapterView, len(adapters))
	for _, adapter := range adapters {
		byName[strings.ToLower(adapter.Interface)] = adapter
	}
	seen := make(map[string]bool, len(interfaces))
	paths := make([]config.Path, 0, len(interfaces))
	for _, name := range interfaces {
		key := strings.ToLower(strings.TrimSpace(name))
		adapter, ok := byName[key]
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		paths = append(paths, config.Path{
			Interface: adapter.Interface,
			LocalAddr: adapter.Address,
			Name:      adapter.Interface,
			Kind:      adapter.Kind,
			Weight:    adapter.Weight,
			MTU:       adapter.MTU,
		})
	}
	if len(paths) == 0 {
		return Profile{}, errors.New("select at least one adapter to bond")
	}
	clientCfg.Paths = paths
	if _, err := clientCfg.Runtime(); err != nil {
		return Profile{}, err
	}

	path := defaultProfileSavePath()
	if path == "" {
		return Profile{}, errors.New("cannot determine where to save the profile")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Profile{}, fmt.Errorf("create profile directory: %w", err)
	}
	if err := config.Save(path, clientCfg); err != nil {
		return Profile{}, err
	}
	a.logger.Info("created client profile",
		"path", path, "server", clientCfg.Server, "adapters", len(paths))
	return a.loadProfile(path)
}

// RefreshAdapters returns the adapters detected right now, for the wizard's
// rescan button.
func (a *App) RefreshAdapters() ([]AdapterView, error) {
	return discoverAdapters()
}

// InstallWintun downloads the signed Wintun DLL next to MultiSpeed.exe, where
// the tun package loads it. No elevation is needed unless the app directory is
// read-only (for example under Program Files).
func (a *App) InstallWintun() (TUNStatus, error) {
	if !wintunStatus().Required {
		return wintunStatus(), nil
	}
	dir, err := executableDir()
	if err != nil {
		return wintunStatus(), err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(wintunZipURL)
	if err != nil {
		return wintunStatus(), fmt.Errorf("download wintun: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return wintunStatus(), fmt.Errorf("download wintun: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWintunZipSize))
	if err != nil {
		return wintunStatus(), fmt.Errorf("download wintun: %w", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return wintunStatus(), fmt.Errorf("open wintun archive: %w", err)
	}
	dll, err := readZipEntry(reader, wintunDLLEntry)
	if err != nil {
		return wintunStatus(), err
	}
	if len(dll) < 2 || dll[0] != 'M' || dll[1] != 'Z' {
		return wintunStatus(), errors.New("the downloaded wintun.dll is not a Windows library")
	}
	if err := writeAtomic(filepath.Join(dir, "wintun.dll"), dll, 0o644); err != nil {
		return wintunStatus(), fmt.Errorf("install wintun.dll (move MultiSpeed to a writable folder or run as administrator): %w", err)
	}
	if license, err := readZipEntry(reader, wintunLicense); err == nil {
		_ = writeAtomic(filepath.Join(dir, "wintun-LICENSE.txt"), license, 0o644)
	}
	a.logger.Info("installed wintun", "path", filepath.Join(dir, "wintun.dll"))
	return wintunStatus(), nil
}

// OpenWintunPage opens the official Wintun download page as a manual fallback.
func (a *App) OpenWintunPage() error {
	if a.ctx == nil {
		return errors.New("application is not ready")
	}
	wailsRuntime.BrowserOpenURL(a.ctx, wintunPage)
	return nil
}

// RestartElevated relaunches MultiSpeed through the Windows UAC prompt.
func (a *App) RestartElevated() error {
	if isElevated() {
		return nil
	}
	if err := relaunchElevated(); err != nil {
		return err
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		wailsRuntime.Quit(a.ctx)
	}()
	return nil
}

func discoverAdapters() ([]AdapterView, error) {
	specs, err := transport.Discover()
	if err != nil {
		return nil, fmt.Errorf("no usable network adapters found: %w", err)
	}
	out := make([]AdapterView, 0, len(specs))
	for _, spec := range specs {
		out = append(out, AdapterView{
			Interface: spec.Interface,
			Address:   spec.LocalAddr,
			Kind:      string(spec.Kind),
			Weight:    spec.Weight,
			MTU:       spec.MTU,
		})
	}
	return out, nil
}

func wintunStatus() TUNStatus {
	status := TUNStatus{
		Platform:    runtime.GOOS,
		DownloadURL: wintunPage,
	}
	if runtime.GOOS != "windows" {
		status.Ready = true
		status.Detail = "not required on this platform"
		return status
	}
	status.Required = true
	dir, err := executableDir()
	if err != nil {
		status.Detail = err.Error()
		return status
	}
	path := filepath.Join(dir, "wintun.dll")
	status.Location = path
	info, err := os.Stat(path)
	if err != nil {
		status.Detail = "wintun.dll was not found next to MultiSpeed.exe"
		return status
	}
	status.Ready = true
	status.Detail = fmt.Sprintf("wintun.dll found (%.0f KB)", float64(info.Size())/1024)
	return status
}

func executableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

func defaultProfileSavePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "MultiSpeed", "client.json")
}

func readZipEntry(reader *zip.Reader, name string) ([]byte, error) {
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		if file.UncompressedSize64 > maxWintunZipSize {
			return nil, fmt.Errorf("%s in the wintun archive is unexpectedly large", name)
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, maxWintunZipSize))
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, fmt.Errorf("the wintun archive does not contain %s", name)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/multispeed-app/multispeed/internal/client"
	"github.com/multispeed-app/multispeed/internal/config"
	"github.com/multispeed-app/multispeed/internal/route"
	"github.com/multispeed-app/multispeed/internal/tun"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	appVersion = "0.1.0-alpha"
	tunName    = "ms0"
)

// Profile is the non-secret summary of a client configuration file.
type Profile struct {
	Path           string   `json:"path"`
	Server         string   `json:"server"`
	Paths          []string `json:"paths"`
	ReorderDelayMS int      `json:"reorder_delay_ms"`
}

// PathView is the UI-facing view of one bonded path.
type PathView struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Weight   int     `json:"weight"`
	Healthy  bool    `json:"healthy"`
	SRTTms   float64 `json:"srtt_ms"`
	JitterMs float64 `json:"jitter_ms"`
	Loss     float64 `json:"loss"`
	RxBPS    uint64  `json:"rx_bps"`
	LastSeen string  `json:"last_seen,omitempty"`
}

// ReorderView reports tunnel reordering counters.
type ReorderView struct {
	Delivered  uint64 `json:"delivered"`
	Duplicates uint64 `json:"duplicates"`
	Late       uint64 `json:"late"`
	Skipped    uint64 `json:"skipped"`
	Pending    int    `json:"pending"`
}

// StatusView is the complete state shown by the desktop app.
type StatusView struct {
	State        string      `json:"state"`
	Version      string      `json:"version"`
	Profile      Profile     `json:"profile"`
	LastError    string      `json:"last_error,omitempty"`
	Server       string      `json:"server,omitempty"`
	TunnelIP     string      `json:"tunnel_ip,omitempty"`
	PrefixLen    int         `json:"prefix_len,omitempty"`
	MTU          int         `json:"mtu,omitempty"`
	ConnectedFor string      `json:"connected_for,omitempty"`
	Reconnecting bool        `json:"reconnecting,omitempty"`
	RetryAttempt int         `json:"retry_attempt,omitempty"`
	Elevated     bool        `json:"elevated"`
	TUNReady     bool        `json:"tun_ready"`
	Paths        []PathView  `json:"paths"`
	Reorder      ReorderView `json:"reorder"`
	DataSeq      uint64      `json:"data_seq"`
	ControlSeq   uint64      `json:"control_seq"`
}

// App is the Wails-bound application backend.
type App struct {
	ctx    context.Context
	log    *logBuffer
	logger *slog.Logger

	mu            sync.Mutex
	state         string
	profile       Profile
	profilePath   string
	session       *activeSession
	connectCancel context.CancelFunc
	lastErr       string
}

// NewApp creates the desktop application backend.
func NewApp() *App {
	buf := newLogBuffer(600)
	return &App{
		log:    buf,
		logger: slog.New(newBufferHandler(buf, slog.LevelInfo)),
		state:  "disconnected",
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.logger.Info("MultiSpeed started", "version", appVersion)

	path := loadProfileSetting()
	if path == "" {
		path = defaultProfilePath()
	}
	if path == "" {
		a.logger.Info("no profile found; choose a client.json to begin")
		return
	}
	profile, err := a.loadProfile(path)
	if err != nil {
		a.logger.Warn("saved profile could not be loaded", "path", path, "error", err)
		return
	}
	a.logger.Info("profile loaded", "server", profile.Server, "paths", len(profile.Paths))
}

func (a *App) shutdown(context.Context) {
	_ = a.Disconnect()
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session != nil {
		select {
		case <-session.done:
		case <-time.After(5 * time.Second):
		}
	}
	a.logger.Info("MultiSpeed stopped")
}

// GetStatus returns a point-in-time snapshot for the UI.
func (a *App) GetStatus() StatusView {
	a.mu.Lock()
	view := StatusView{
		State:     a.state,
		Version:   appVersion,
		Profile:   a.profile,
		LastError: a.lastErr,
		Elevated:  isElevated(),
		TUNReady:  wintunStatus().Ready,
	}
	session := a.session
	a.mu.Unlock()

	if session == nil {
		return view
	}
	status := session.client.Status()
	if view.State == "connected" && status.Reconnecting {
		view.State = "reconnecting"
	}
	view.Reconnecting = status.Reconnecting
	view.RetryAttempt = status.ReconnectAttempt
	view.Server = status.Server.String()
	view.TunnelIP = status.Session.Address.String()
	view.PrefixLen = int(status.Session.PrefixLen)
	view.MTU = int(status.Session.MTU)
	view.DataSeq = status.DataSeq
	view.ControlSeq = status.ControlSeq
	view.ConnectedFor = time.Since(session.started).Round(time.Second).String()
	view.Reorder = ReorderView{
		Delivered:  status.Reorder.Delivered,
		Duplicates: status.Reorder.Duplicates,
		Late:       status.Reorder.Late,
		Skipped:    status.Reorder.Skipped,
		Pending:    status.Reorder.Pending,
	}
	view.Paths = make([]PathView, 0, len(status.Paths))
	for _, p := range status.Paths {
		path := PathView{
			ID:       int(p.ID),
			Name:     p.Name,
			Kind:     string(p.Kind),
			Weight:   p.Weight,
			Healthy:  p.Healthy,
			SRTTms:   p.SRTTms,
			JitterMs: p.Jitterms,
			Loss:     p.Loss,
			RxBPS:    p.RxBPS,
		}
		if !p.LastSeen.IsZero() {
			path.LastSeen = p.LastSeen.Format(time.RFC3339)
		}
		view.Paths = append(view.Paths, path)
	}
	return view
}

// GetLogs returns log lines with a sequence number greater than since.
func (a *App) GetLogs(since int64) []LogLine {
	return a.log.snapshot(since)
}

// LoadProfile reads and validates a client configuration file without exposing
// its private key to the frontend.
func (a *App) LoadProfile(path string) (Profile, error) {
	profile, err := a.loadProfile(path)
	if err != nil {
		a.logger.Warn("profile load failed", "error", err)
		return Profile{}, err
	}
	a.logger.Info("profile loaded", "server", profile.Server, "paths", len(profile.Paths))
	return profile, nil
}

func (a *App) loadProfile(path string) (Profile, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Profile{}, errors.New("no profile path given")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != "disconnected" {
		return Profile{}, errors.New("disconnect before loading another profile")
	}
	profile, err := readProfile(path)
	if err != nil {
		return Profile{}, err
	}
	a.profile = profile
	a.profilePath = profile.Path
	a.lastErr = ""
	if err := saveProfileSetting(profile.Path); err != nil {
		a.logger.Warn("could not remember profile path", "error", err)
	}
	return profile, nil
}

// ChooseProfile opens a native file picker and loads the selected profile.
func (a *App) ChooseProfile() (Profile, error) {
	if a.ctx == nil {
		return Profile{}, errors.New("application is not ready")
	}
	options := runtime.OpenDialogOptions{
		Title: "Choose a MultiSpeed client profile",
		Filters: []runtime.FileFilter{
			{DisplayName: "MultiSpeed client profile (*.json)", Pattern: "*.json"},
			{DisplayName: "All files", Pattern: "*.*"},
		},
	}
	a.mu.Lock()
	if a.profilePath != "" {
		options.DefaultDirectory = filepath.Dir(a.profilePath)
	}
	a.mu.Unlock()
	path, err := runtime.OpenFileDialog(a.ctx, options)
	if err != nil {
		return Profile{}, err
	}
	if path == "" {
		return a.GetStatus().Profile, nil
	}
	return a.LoadProfile(path)
}

// Connect establishes the tunnel and configures host routes and DNS.
func (a *App) Connect() error {
	a.mu.Lock()
	if a.state != "disconnected" {
		state := a.state
		a.mu.Unlock()
		return fmt.Errorf("cannot connect while %s", state)
	}
	path := a.profilePath
	a.mu.Unlock()
	if path == "" {
		return errors.New("choose a client profile first")
	}

	var file config.Client
	if err := config.Load(path, &file); err != nil {
		return a.failConnect(err)
	}
	cfg, err := file.Runtime()
	if err != nil {
		return a.failConnect(err)
	}
	cfg.Logger = a.logger

	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	session := &activeSession{ctx: ctx, cancel: cancel, done: make(chan struct{}), started: time.Now()}

	a.mu.Lock()
	if a.state != "disconnected" {
		a.mu.Unlock()
		cancel()
		return fmt.Errorf("cannot connect while %s", a.state)
	}
	a.state = "connecting"
	a.connectCancel = cancel
	a.lastErr = ""
	a.mu.Unlock()
	a.logger.Info("connecting", "server", cfg.Server, "profile", filepath.Base(path))

	pc, err := client.Dial(ctx, cfg)
	if err != nil {
		cancel()
		return a.failConnect(err)
	}
	session.client = pc
	info := pc.Info()

	dev, err := tun.Open(tunName, int(info.MTU))
	if err != nil {
		_ = pc.Close()
		cancel()
		return a.failConnect(fmt.Errorf("open TUN device: %w (run as administrator and place wintun.dll next to the app)", err))
	}
	session.dev = dev

	manager, err := route.Configure(ctx, route.Config{
		TUNName:            dev.Name(),
		Address:            info.Address,
		PrefixLen:          info.PrefixLen,
		MTU:                int(info.MTU),
		Server:             pc.Server().Addr(),
		PhysicalInterfaces: pc.PhysicalInterfaces(),
		DNS:                info.DNS,
		ConfigureDNS:       info.DNS.IsValid(),
	})
	if err != nil {
		_ = dev.Close()
		_ = pc.Close()
		cancel()
		return a.failConnect(fmt.Errorf("configure routes: %w", err))
	}
	session.routes = manager

	if err := ctx.Err(); err != nil {
		_ = manager.Close()
		_ = dev.Close()
		_ = pc.Close()
		cancel()
		return a.failConnect(errors.New("connection cancelled"))
	}

	a.mu.Lock()
	a.session = session
	a.state = "connected"
	a.connectCancel = nil
	a.mu.Unlock()

	for _, warning := range manager.Warnings() {
		a.logger.Warn("route setup warning", "error", warning)
	}
	a.logger.Info("connected",
		"tunnel_ip", info.Address.String(),
		"mtu", info.MTU,
		"paths", len(pc.Status().Paths),
		"adapters", strings.Join(pc.PhysicalInterfaces(), ", "),
	)
	go a.runSession(session)
	return nil
}

// Disconnect tears down an active session and restores host networking.
func (a *App) Disconnect() error {
	a.mu.Lock()
	if a.state == "connecting" {
		cancel := a.connectCancel
		a.state = "disconnecting"
		a.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
	session := a.session
	if session == nil || a.state == "disconnecting" {
		a.mu.Unlock()
		return nil
	}
	a.state = "disconnecting"
	a.mu.Unlock()

	a.logger.Info("disconnecting")
	session.cancel()
	select {
	case <-session.done:
	case <-time.After(10 * time.Second):
		a.logger.Warn("disconnect is taking longer than expected")
	}
	return nil
}

func (a *App) failConnect(err error) error {
	a.mu.Lock()
	a.state = "disconnected"
	a.connectCancel = nil
	a.lastErr = err.Error()
	a.mu.Unlock()
	a.logger.Error("connect failed", "error", err)
	return err
}

func readProfile(path string) (Profile, error) {
	var file config.Client
	if err := config.Load(path, &file); err != nil {
		return Profile{}, err
	}
	cfg, err := file.Runtime()
	if err != nil {
		return Profile{}, err
	}
	names := make([]string, 0, len(cfg.Paths))
	for i, p := range cfg.Paths {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = strings.TrimSpace(p.Interface)
		}
		if name == "" {
			name = fmt.Sprintf("path-%d", i+1)
		}
		if p.Kind != "" {
			name += " (" + string(p.Kind) + ")"
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		names = []string{"auto-discover"}
	}
	return Profile{
		Path:           path,
		Server:         cfg.Server,
		Paths:          names,
		ReorderDelayMS: int(cfg.Reorder.MaxDelay / time.Millisecond),
	}, nil
}

func defaultProfilePath() string {
	candidates := make([]string, 0, 4)
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "client.json"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "client.json"))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "MultiSpeed", "client.json"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "MultiSpeed", "settings.json"), nil
}

func loadProfileSetting() string {
	path, err := settingsPath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var settings struct {
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return ""
	}
	return settings.Profile
}

func saveProfileSetting(profile string) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Profile string `json:"profile"`
	}{Profile: profile}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

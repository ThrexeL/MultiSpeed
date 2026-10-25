package main

import (
	"os"
	"testing"

	"github.com/multispeed-app/multispeed/internal/config"
)

func TestPreviewAndSaveSetup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	relayKeys, err := config.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	clientKeys, err := config.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	code, err := config.NewJoinCode("203.0.113.9:7443", relayKeys.PublicKey, clientKeys.PrivateKey, "test-client")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := config.EncodeJoinCode(code)
	if err != nil {
		t.Fatal(err)
	}

	app := NewApp()
	preview, err := app.PreviewJoin(encoded)
	if err != nil {
		t.Skipf("no usable adapters on this machine: %v", err)
	}
	if preview.Server != "203.0.113.9:7443" || preview.ClientName != "test-client" {
		t.Fatalf("preview = %+v", preview)
	}
	if len(preview.Adapters) == 0 {
		t.Skip("no usable adapters on this machine")
	}

	var selected []string
	for _, adapter := range preview.Adapters {
		selected = append(selected, adapter.Interface)
	}
	profile, err := app.SaveSetup(encoded, selected)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Server != preview.Server {
		t.Fatalf("profile server = %q", profile.Server)
	}
	if len(profile.Paths) != len(selected) {
		t.Fatalf("profile paths = %v, want %d entries", profile.Paths, len(selected))
	}

	path := defaultProfileSavePath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("profile was not written to %s: %v", path, err)
	}
	var saved config.Client
	if err := config.Load(path, &saved); err != nil {
		t.Fatalf("saved profile is not readable: %v", err)
	}
	if len(saved.Paths) != len(selected) {
		t.Fatalf("saved paths = %v", saved.Paths)
	}

	if _, err := app.SaveSetup(encoded, []string{"does-not-exist"}); err == nil {
		t.Fatal("saved a profile with no real adapters")
	}
}

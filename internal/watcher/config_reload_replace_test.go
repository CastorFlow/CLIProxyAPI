package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// A config is commonly replaced by writing a temporary file and renaming it over
// the target (CPA's own saves and external editors such as the KeyManageCenter
// panel do this). A watch held on the config file follows its inode, so after the
// first replacement it never sees another event for the config path. The
// directory watch must keep the reload path working.
func TestConfigReloadSurvivesAtomicReplace(t *testing.T) {
	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"), 0o644); err != nil {
		t.Fatalf("create config: %v", err)
	}

	var reloads atomic.Int32
	w, err := NewWatcher(configPath, authDir, func(*config.Config) { reloads.Add(1) })
	if err != nil {
		t.Fatalf("create watcher: %v", err)
	}
	w.SetConfig(&config.Config{AuthDir: authDir})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	defer func() { _ = w.Stop() }()

	waitForReloads := func(target int32) bool {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if reloads.Load() >= target {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	if !waitForReloads(1) {
		t.Fatal("initial reload did not run")
	}

	// Two replacements in a row: the first detaches the file watch, the second
	// proves the directory watch keeps reporting the config path.
	for round := int32(2); round <= 3; round++ {
		tmpFile := filepath.Join(tmpDir, ".config-replace-tmp")
		body := []byte("auth-dir: " + authDir + "\nrequest-retry: " + string(rune('0'+round)) + "\n")
		if err := os.WriteFile(tmpFile, body, 0o644); err != nil {
			t.Fatalf("write replacement %d: %v", round, err)
		}
		if err := os.Rename(tmpFile, configPath); err != nil {
			t.Fatalf("rename replacement %d: %v", round, err)
		}
		if !waitForReloads(round) {
			t.Fatalf("atomic replace %d did not trigger a config reload (reloads=%d)", round, reloads.Load())
		}
	}
}

// The directory watch must not turn unrelated sibling files into config reloads.
func TestConfigReloadIgnoresSiblingFileWrites(t *testing.T) {
	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"), 0o644); err != nil {
		t.Fatalf("create config: %v", err)
	}

	var reloads atomic.Int32
	w, err := NewWatcher(configPath, authDir, func(*config.Config) { reloads.Add(1) })
	if err != nil {
		t.Fatalf("create watcher: %v", err)
	}
	w.SetConfig(&config.Config{AuthDir: authDir})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	defer func() { _ = w.Stop() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && reloads.Load() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	base := reloads.Load()

	// A backup file and a temp file next to the config are not config changes.
	if err := os.WriteFile(configPath+".bak.key-manage-center-20260101-000000", []byte("noise\n"), 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if got := reloads.Load(); got != base {
		t.Fatalf("sibling file write triggered %d unexpected reload(s)", got-base)
	}
}

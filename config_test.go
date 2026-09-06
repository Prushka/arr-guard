package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func cleanConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"DRY_RUN", "RECOVER_BLOCKED_QUEUE", "MAX_ATTEMPTS", "WORKERS"} {
		old, ok := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if ok {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	for key, value := range map[string]string{"MODE": "subtitles", "SONARR_URL": "http://example.invalid/base", "SONARR_API_KEY": "test", "RADARR_URL": "", "RADARR_API_KEY": "", "SONARR_API_VERSION": "v3", "WEBHOOK_TOKEN": "", "WEBHOOK_USERNAME": "", "WEBHOOK_PASSWORD": "", "PATH_MAPPINGS_JSON": "", "STATE_PATH": filepath.Join(t.TempDir(), "state.json"), "UNMATCHED_PATH": filepath.Join(t.TempDir(), "unmatched.json")} {
		t.Setenv(key, value)
	}
}

func TestConfigSafeDefaultsAndRejectsMalformedInputs(t *testing.T) {
	cleanConfigEnv(t)
	cfg, err := LoadConfig()
	if err != nil || !cfg.DryRun || cfg.RecoverBlockedQueue {
		t.Fatalf("unsafe defaults: %v", err)
	}
	for _, test := range []struct{ key, value string }{
		{"DRY_RUN", "tru"}, {"DRY_RUN", ""}, {"RECOVER_BLOCKED_QUEUE", "on"}, {"WORKERS", "many"}, {"WORKERS", "0"}, {"WORKERS", "129"}, {"MAX_ATTEMPTS", "3.5"}, {"MAX_ATTEMPTS", "0"}, {"MAX_ATTEMPTS", "1001"}, {"SONARR_API_VERSION", "v4"},
		{"SONARR_URL", "file:///data"}, {"SONARR_URL", "http://user:password@host"}, {"SONARR_URL", "http://host?apikey=secret"}, {"SONARR_URL", "http://host#fragment"}, {"MODE", "invalid"},
		{"STATE_PATH", "media.mkv"}, {"UNMATCHED_PATH", "media.srt"},
	} {
		t.Run(test.key+test.value, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("accepted malformed setting")
			}
		})
	}
}

func TestConfigOutputPathsAndAuthentication(t *testing.T) {
	cleanConfigEnv(t)
	t.Setenv("MODE", "serve")
	t.Setenv("DRY_RUN", "false")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("write-enabled service accepted no authentication")
	}
	t.Setenv("WEBHOOK_TOKEN", "test")
	if _, err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UNMATCHED_PATH", os.Getenv("STATE_PATH"))
	if _, err := LoadConfig(); err == nil {
		t.Fatal("outputs can overwrite each other")
	}
	root := t.TempDir()
	data, err := json.Marshal([]PathMapping{{From: "/media", To: root}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH_MAPPINGS_JSON", string(data))
	t.Setenv("STATE_PATH", filepath.Join(root, "state.json"))
	t.Setenv("UNMATCHED_PATH", filepath.Join(t.TempDir(), "report.json"))
	if _, err := LoadConfig(); err == nil {
		t.Fatal("state output allowed inside media")
	}
}

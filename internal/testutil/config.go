package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func CleanConfigEnv(t *testing.T) {
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

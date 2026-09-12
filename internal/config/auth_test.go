package config

import (
	"testing"
)

func TestLoadConfigRequiresBasicAuthPair(t *testing.T) {
	t.Setenv("SONARR_URL", "http://sonarr.invalid")
	t.Setenv("SONARR_API_KEY", "key")
	t.Setenv("RADARR_URL", "")
	t.Setenv("RADARR_API_KEY", "")
	t.Setenv("WEBHOOK_TOKEN", "")
	t.Setenv("WEBHOOK_USERNAME", "guard")
	t.Setenv("WEBHOOK_PASSWORD", "")
	if _, err := Load(); err == nil {
		t.Fatal("LoadConfig accepted only one Basic Auth credential")
	}
}

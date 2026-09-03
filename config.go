package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Mode                 string
	ListenAddr           string
	WebhookToken         string
	WebhookUsername      string
	WebhookPassword      string
	FFprobePath          string
	StatePath            string
	UnmatchedPath        string
	MaxAttempts          int
	Workers              int
	DryRun               bool
	PathMappings         []PathMapping
	UnmatchedExcludeDirs []string
	Sonarr               *ArrConfig
	Radarr               *ArrConfig
}

type ArrConfig struct {
	Name       string
	Kind       string
	URL        string
	APIKey     string
	APIVersion string
}

type PathMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Mode:                 strings.ToLower(envOr("MODE", "serve")),
		ListenAddr:           envOr("LISTEN_ADDR", ":8080"),
		WebhookToken:         strings.TrimSpace(os.Getenv("WEBHOOK_TOKEN")),
		WebhookUsername:      strings.TrimSpace(os.Getenv("WEBHOOK_USERNAME")),
		WebhookPassword:      os.Getenv("WEBHOOK_PASSWORD"),
		FFprobePath:          envOr("FFPROBE_PATH", "ffprobe"),
		StatePath:            envOr("STATE_PATH", "./state.json"),
		UnmatchedPath:        envOr("UNMATCHED_PATH", "./unmatched.json"),
		MaxAttempts:          envInt("MAX_ATTEMPTS", 3),
		Workers:              envInt("WORKERS", 2),
		DryRun:               envBool("DRY_RUN", false),
		UnmatchedExcludeDirs: envCSVPaths("UNMATCHED_EXCLUDE_DIRS"),
	}
	switch cfg.Mode {
	case "serve", "unmatched", "subtitles":
	default:
		return Config{}, fmt.Errorf("MODE must be serve, unmatched, or subtitles (got %q)", cfg.Mode)
	}
	if cfg.MaxAttempts < 1 {
		return Config{}, errors.New("MAX_ATTEMPTS must be at least 1")
	}
	if cfg.Workers < 1 {
		return Config{}, errors.New("WORKERS must be at least 1")
	}
	if (cfg.WebhookUsername == "") != (cfg.WebhookPassword == "") {
		return Config{}, errors.New("WEBHOOK_USERNAME and WEBHOOK_PASSWORD must be set together")
	}

	if raw := strings.TrimSpace(os.Getenv("PATH_MAPPINGS_JSON")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.PathMappings); err != nil {
			return Config{}, fmt.Errorf("PATH_MAPPINGS_JSON: %w", err)
		}
		for i, mapping := range cfg.PathMappings {
			if strings.TrimSpace(mapping.From) == "" || strings.TrimSpace(mapping.To) == "" {
				return Config{}, fmt.Errorf("PATH_MAPPINGS_JSON[%d] requires from and to", i)
			}
		}
	}

	sonarr, err := loadArrConfig("SONARR", "Sonarr", "sonarr")
	if err != nil {
		return Config{}, err
	}
	radarr, err := loadArrConfig("RADARR", "Radarr", "radarr")
	if err != nil {
		return Config{}, err
	}
	cfg.Sonarr = sonarr
	cfg.Radarr = radarr
	if cfg.Sonarr == nil && cfg.Radarr == nil {
		return Config{}, errors.New("configure at least one of SONARR_URL/RADARR_URL")
	}
	return cfg, nil
}

func loadArrConfig(prefix, name, kind string) (*ArrConfig, error) {
	url := strings.TrimRight(strings.TrimSpace(os.Getenv(prefix+"_URL")), "/")
	key := strings.TrimSpace(os.Getenv(prefix + "_API_KEY"))
	if url == "" && key == "" {
		return nil, nil
	}
	if url == "" || key == "" {
		return nil, fmt.Errorf("%s_URL and %s_API_KEY must be set together", prefix, prefix)
	}
	return &ArrConfig{
		Name:       name,
		Kind:       kind,
		URL:        url,
		APIKey:     key,
		APIVersion: envOr(prefix+"_API_VERSION", "v3"),
	}, nil
}

func (c Config) logValue() slog.Value {
	return slog.GroupValue(
		slog.String("mode", c.Mode),
		slog.String("listen", c.ListenAddr),
		slog.String("ffprobe", c.FFprobePath),
		slog.String("unmatched_path", c.UnmatchedPath),
		slog.Int("workers", c.Workers),
		slog.Int("max_attempts", c.MaxAttempts),
		slog.Bool("dry_run", c.DryRun),
	)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return value
}

func envCSVPaths(key string) []string {
	values := make([]string, 0)
	for _, raw := range strings.Split(os.Getenv(key), ",") {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return values
}

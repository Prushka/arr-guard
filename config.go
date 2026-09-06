package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
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
	RecoverBlockedQueue  bool
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
	for _, key := range []string{"DRY_RUN", "RECOVER_BLOCKED_QUEUE"} {
		if value, ok := os.LookupEnv(key); ok {
			if _, err := strconv.ParseBool(strings.TrimSpace(value)); err != nil {
				return Config{}, fmt.Errorf("%s must be a boolean", key)
			}
		}
	}
	for _, key := range []string{"WORKERS", "MAX_ATTEMPTS"} {
		if value, ok := os.LookupEnv(key); ok {
			if _, err := strconv.Atoi(strings.TrimSpace(value)); err != nil {
				return Config{}, fmt.Errorf("%s must be an integer", key)
			}
		}
	}
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
		DryRun:               envBool("DRY_RUN", true),
		RecoverBlockedQueue:  envBool("RECOVER_BLOCKED_QUEUE", false),
		UnmatchedExcludeDirs: envCSVPaths("UNMATCHED_EXCLUDE_DIRS"),
	}
	switch cfg.Mode {
	case "serve", "unmatched", "subtitles":
	default:
		return Config{}, fmt.Errorf("MODE must be serve, unmatched, or subtitles (got %q)", cfg.Mode)
	}
	if cfg.MaxAttempts < 1 || cfg.MaxAttempts > 1000 {
		return Config{}, errors.New("MAX_ATTEMPTS must be between 1 and 1000")
	}
	if cfg.Workers < 1 || cfg.Workers > 128 {
		return Config{}, errors.New("WORKERS must be between 1 and 128")
	}
	if (cfg.WebhookUsername == "") != (cfg.WebhookPassword == "") {
		return Config{}, errors.New("WEBHOOK_USERNAME and WEBHOOK_PASSWORD must be set together")
	}
	if cfg.Mode == "serve" && !cfg.DryRun && cfg.WebhookToken == "" && cfg.WebhookUsername == "" {
		return Config{}, errors.New("write-enabled serve mode requires webhook authentication")
	}

	if raw := strings.TrimSpace(os.Getenv("PATH_MAPPINGS_JSON")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.PathMappings); err != nil {
			return Config{}, fmt.Errorf("PATH_MAPPINGS_JSON: %w", err)
		}
		for i, mapping := range cfg.PathMappings {
			if strings.TrimSpace(mapping.From) == "" || strings.TrimSpace(mapping.To) == "" {
				return Config{}, fmt.Errorf("PATH_MAPPINGS_JSON[%d] requires from and to", i)
			}
			if !filepath.IsAbs(mapping.To) {
				return Config{}, fmt.Errorf("PATH_MAPPINGS_JSON[%d].to must be an absolute local path", i)
			}
			from := normalizePath(mapping.From)
			if !strings.HasPrefix(from, "/") && (len(from) <= 2 || from[1] != ':' || from[2] != '/') {
				return Config{}, fmt.Errorf("PATH_MAPPINGS_JSON[%d].from must be an absolute Arr path", i)
			}
			for _, previous := range cfg.PathMappings[:i] {
				one, two := comparableArrPath(previous.From, mapping.From)
				if one == two {
					return Config{}, errors.New("duplicate source path mappings are ambiguous")
				}
			}
		}
	}
	if scanPathKey(resolveExistingPath(cfg.StatePath)) == scanPathKey(resolveExistingPath(cfg.UnmatchedPath)) {
		return Config{}, errors.New("STATE_PATH and UNMATCHED_PATH must differ")
	}
	for _, output := range []string{cfg.StatePath, cfg.UnmatchedPath} {
		if !strings.EqualFold(filepath.Ext(output), ".json") {
			return Config{}, errors.New("state/report output must have a .json extension")
		}
		if info, err := os.Lstat(output); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
			return Config{}, errors.New("state/report output must be a regular file, not a symlink")
		}
		if isMediaPath(output) || isSubtitlePath(output) {
			return Config{}, errors.New("state/report output must not use a media or subtitle extension")
		}
		for _, mapping := range cfg.PathMappings {
			if pathWithin(resolveExistingPath(mapping.To), resolveExistingPath(output)) {
				return Config{}, errors.New("state/report output must be outside mapped media roots")
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
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv(prefix+"_URL")), "/")
	key := strings.TrimSpace(os.Getenv(prefix + "_API_KEY"))
	if baseURL == "" && key == "" {
		return nil, nil
	}
	if baseURL == "" || key == "" {
		return nil, fmt.Errorf("%s_URL and %s_API_KEY must be set together", prefix, prefix)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%s_URL must be an HTTP(S) base URL without credentials, query, or fragment", prefix)
	}
	if envOr(prefix+"_API_VERSION", "v3") != "v3" {
		return nil, fmt.Errorf("%s_API_VERSION must be v3", prefix)
	}
	return &ArrConfig{
		Name:       name,
		Kind:       kind,
		URL:        baseURL,
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

package guard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Prushka/arr-guard/internal/config"
	"github.com/Prushka/arr-guard/internal/probe"
	"github.com/Prushka/arr-guard/internal/testutil"
)

func TestRunDryModesAndBindFailureNeverMutate(t *testing.T) {
	for _, mode := range []string{"subtitles", "unmatched", "serve"} {
		t.Run(mode, func(t *testing.T) {
			testutil.CleanConfigEnv(t)
			f := newSafetyFixture(t, "radarr")
			f.file.Year = time.Now().Year() - 51
			t.Setenv("SONARR_URL", "")
			t.Setenv("SONARR_API_KEY", "")
			t.Setenv("RADARR_URL", f.client.BaseURL())
			t.Setenv("RADARR_API_KEY", "test")
			t.Setenv("RADARR_API_VERSION", "v3")
			t.Setenv("DRY_RUN", "true")
			t.Setenv("MODE", mode)
			mappings, err := json.Marshal([]config.PathMapping{{From: filepath.Dir(f.file.Path), To: filepath.Dir(f.file.Path)}})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH_MAPPINGS_JSON", string(mappings))
			var occupied net.Listener
			if mode == "serve" {
				occupied, err = net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = occupied.Close() }()
				t.Setenv("LISTEN_ADDR", occupied.Addr().String())
			}
			err = Run(t.Context(), nil)
			if mode == "serve" {
				if err == nil {
					t.Fatal("expected bind failure")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(f.mutations) > 0 {
				t.Fatal("read-only run mutated Arr")
			}
			if _, err := os.Stat(os.Getenv("STATE_PATH")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("read-only run wrote state")
			}
		})
	}
}

func TestServingHealthAndCancellation(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	f.service.config.DryRun = true
	f.service.jobs = make(chan webhookJob, 4)
	f.service.stop = make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.service.serveOnListener(ctx, listener) }()
	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("health endpoint unavailable")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop")
	}
	if len(f.mutations) > 0 {
		t.Fatal("dry-run serve mutated Arr")
	}
}

func TestAuditContinuesAcrossInstances(t *testing.T) {
	sonarr := newSafetyFixture(t, "sonarr")
	radarr := newSafetyFixture(t, "radarr")
	sonarr.service.arr["radarr"] = radarr.client
	sonarr.service.config.DryRun = true
	sonarr.service.probeFn = func(_ context.Context, path string) (probe.Validation, error) {
		if path == sonarr.file.Path {
			return probe.Validation{}, errors.New("injected probe failure")
		}
		v := radarr.validation
		v.Valid = true
		return v, nil
	}
	var radarrRead bool
	radarr.before = func(r *http.Request) {
		if r.URL.Path == "/api/v3/moviefile/17" {
			radarrRead = true
		}
	}
	if err := sonarr.service.Audit(t.Context()); err == nil {
		t.Fatal("audit hid failed probe")
	}
	if !radarrRead {
		t.Fatal("one instance failure prevented other instance scan")
	}
	if len(sonarr.mutations)+len(radarr.mutations) > 0 {
		t.Fatal("audit mutated in dry run")
	}
}

func TestUnmatchedFollowsConfiguredRootAliasWithoutFalseOrphans(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	root := filepath.Dir(f.file.Path)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip("directory symlinks are unavailable on this host")
	}
	f.service.config.PathMappings = []config.PathMapping{{From: root, To: alias}}
	f.service.config.UnmatchedPath = filepath.Join(t.TempDir(), "report.json")
	if err := f.service.ScanUnmatched(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.service.config.UnmatchedPath)
	if err != nil {
		t.Fatal(err)
	}
	var report UnmatchedReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Files) != 0 || len(report.Roots) != 1 {
		t.Fatal("root alias produced false orphan report")
	}
}

func TestNewServiceEnforcesReadOnlyClientAndExclusiveState(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, mode := range []string{"serve", "subtitles", "unmatched"} {
		cfg := config.Config{Mode: mode, DryRun: true, Workers: 1, StatePath: filepath.Join(t.TempDir(), "state.json"), Radarr: &config.Arr{Kind: "radarr", Name: "radarr", URL: "http://example.invalid", APIVersion: "v3"}}
		s, err := NewService(cfg, log)
		if err != nil {
			t.Fatal(err)
		}
		if !s.arr["radarr"].IsReadOnly() {
			t.Fatal("service did not set client read-only")
		}
		_ = s.state.Close()
	}
	cfg := config.Config{Mode: "serve", Workers: 1, StatePath: filepath.Join(t.TempDir(), "state.json"), Radarr: &config.Arr{Kind: "radarr", Name: "radarr", URL: "http://example.invalid", APIVersion: "v3"}}
	s, err := NewService(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.state.Close() }()
	if _, err := NewService(cfg, log); err == nil {
		t.Fatal("second writer started")
	}
	if err := Run(t.Context(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), []string{"invalid"}); err == nil {
		t.Fatal("invalid command line accepted")
	}
}

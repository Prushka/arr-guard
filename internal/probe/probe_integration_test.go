package probe

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRealFFprobeSubtitleMatrix(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required for real probe fixture generation")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required for real probe test")
	}
	for _, test := range []struct {
		name, language, sidecar string
		valid, hasSubtitles     bool
	}{
		{"none", "", "", false, false},
		{"embeddedEnglish", "eng", "", true, true},
		{"embeddedJapanese", "jpn", "", false, true},
		{"embeddedUnknown", "und", "", false, true},
		{"englishSidecar", "", "en", true, true},
		{"unrelatedSidecar", "", "unrelated", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			media := filepath.Join(dir, "Movie.mkv")
			sub := filepath.Join(dir, "input.srt")
			if err := os.WriteFile(sub, []byte("1\n00:00:00,000 --> 00:00:00,800\nTest subtitle\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=c=black:s=16x16:d=1"}
			if test.language != "" {
				args = append(args, "-i", sub, "-map", "0:v", "-map", "1:s", "-c:s", "srt", "-metadata:s:s:0", "language="+test.language)
			}
			args = append(args, "-t", "1", "-c:v", "ffv1", media)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture generation: %v: %s", err, out)
			}
			if test.sidecar != "" {
				name := "Movie.en.srt"
				if test.sidecar == "unrelated" {
					name = "OtherMovie.en.srt"
				}
				data, err := os.ReadFile(sub)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p := Prober{Path: ffprobe, Timeout: 10 * time.Second}
			reportPath := filepath.Join(dir, "probe-report.log")
			t.Setenv("FFREPORT", "file="+strings.ReplaceAll(filepath.ToSlash(reportPath), ":", `\:`))
			t.Setenv("AV_LOG_FORCE_COLOR", "1")
			v, err := p.Validate(t.Context(), media)
			if err != nil {
				t.Fatal(err)
			}
			if v.Valid != test.valid || v.HasSubtitles != test.hasSubtitles || v.FileInfo == nil || v.DirectoryInfo == nil {
				t.Fatalf("unexpected validation: %+v", v)
			}
			if _, err := os.Stat(reportPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("read-only probe created an implicit report")
			}
		})
	}
}

func TestProbeRejectsInvalidInputAndEmptySidecars(t *testing.T) {
	p := Prober{Path: "unused"}
	for _, path := range []string{"relative.mkv", "https://example.invalid/media.mkv", t.TempDir(), filepath.Join(t.TempDir(), "missing.mkv")} {
		if _, err := p.Validate(t.Context(), path); err == nil {
			t.Fatal("invalid probe input accepted")
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Movie.en.srt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverExternalSubtitles(filepath.Join(dir, "Movie.mkv")); err == nil {
		t.Fatal("empty subtitle accepted")
	}
	if got := subtitleStreamLanguage(map[string]string{"LANGUAGE": "eng"}); got != "en" {
		t.Fatal("uppercase metadata not recognized")
	}
	b := limitedBuffer{limit: 4}
	if _, err := b.Write([]byte("12345")); err == nil {
		t.Fatal("probe output exceeded limit")
	}
	if _, err := io.Copy(&b, strings.NewReader("12345")); err == nil {
		t.Fatal("io.Copy bypassed bounded probe output")
	}
}

func TestUnicodeSubtitlePrefixCannotPanic(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, strings.Repeat("\u0130", 20)+".mkv")
	if err := os.WriteFile(filepath.Join(dir, strings.Repeat("i", 20)+".en.srt"), []byte("subtitle"), 0o600); err != nil {
		t.Fatal(err)
	}
	subs, err := DiscoverExternalSubtitles(media)
	if err != nil || len(subs) != 1 || subs[0].Language != "en" {
		t.Fatal("Unicode prefix was sliced incorrectly", err)
	}
}

func TestProbeFailuresNeverBecomeSubtitleRejections(t *testing.T) {
	for _, test := range []struct {
		name, output, diagnostic string
	}{
		{name: "malformed", output: "invalid-json"},
		{name: "null", output: "null"},
		{name: "missingStreams", output: "{}"},
		{name: "mediaErrorDespiteSuccess", output: `{"streams":[]}`, diagnostic: "fixture corrupt media"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			media := filepath.Join(dir, "fixture.mkv")
			if err := os.WriteFile(media, []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			probe := filepath.Join(dir, "probe")
			body := "#!/bin/sh\nprintf '%s' '" + test.output + "'\n"
			if test.diagnostic != "" {
				body += "printf '%s' '" + test.diagnostic + "' >&2\n"
			}
			if runtime.GOOS == "windows" {
				probe += ".cmd"
				body = "@echo off\r\necho " + test.output + "\r\n"
				if test.diagnostic != "" {
					body += "echo " + test.diagnostic + " 1>&2\r\n"
				}
			}
			if err := os.WriteFile(probe, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := (Prober{Path: probe, Timeout: time.Second}).Validate(t.Context(), media); err == nil {
				t.Fatal("untrustworthy probe output became a subtitle decision")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := (Prober{Path: probe}).Validate(ctx, media); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled probe lost cancellation identity", err)
			}
			if _, err := (Prober{Path: probe, Timeout: time.Nanosecond}).Validate(t.Context(), media); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("probe deadline did not stop validation", err)
			}
		})
	}
}

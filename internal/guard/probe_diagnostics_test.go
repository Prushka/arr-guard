package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Prushka/arr-guard/internal/probe"
)

const recoveredVideoJSON = `{"codec_type":"video","codec_name":"mpeg2video","width":1440,"height":1080}`

const recoveredVideoError = "[mpeg2video @ 000001A1234] [error] Invalid frame dimensions 0x0."

func nonSubtitleDiagnosticFixtures() []struct{ name, streams, format, diagnostic string } {
	return []struct{ name, streams, format, diagnostic string }{
		{"MPEG2", recoveredVideoJSON, "mpegts", recoveredVideoError},
		{"ZeroDimensionVideo", `{"codec_type":"video","codec_name":"mpeg2video"}`, "mpegts", "[mpeg2video @ 0x123] [error] slice mismatch"},
		{"Chapters", `{"codec_type":"video","codec_name":"h264"}`, "matroska,webm", "[matroska,webm @ 0x123] [error] Chapter end time 0 before start 40040000000\n[matroska,webm @ 0x123] [error] Chapter end time 0 before start 130297000000"},
		{"JPEG", `{"codec_type":"video","codec_name":"h264"},{"codec_type":"video","codec_name":"mjpeg","disposition":{"attached_pic":1}}`, "matroska,webm", "[mjpeg @ 0x123] [error] dqt: invalid precision\n[mjpeg @ 0x123] [error] unable to decode APP fields: Invalid data found when processing input\n[mjpeg @ 0x123] [fatal] No JPEG data found in image"},
		{"Audio", `{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"aac"}`, "matroska,webm", "[aac @ 0x123] [error] Error decoding AAC frame header."},
	}
}

func fixtureDiagnosticProber(t *testing.T, output, diagnostic string, exitCode int) probe.Prober {
	t.Helper()
	dir := t.TempDir()
	outPath, errPath := filepath.Join(dir, "stdout.json"), filepath.Join(dir, "stderr.txt")
	for path, text := range map[string]string{outPath: output, errPath: diagnostic} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	probePath := filepath.Join(dir, "probe")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	body := fmt.Sprintf("#!/bin/sh\ncat %s\ncat %s >&2\nexit %d\n", quote(outPath), quote(errPath), exitCode)
	if runtime.GOOS == "windows" {
		probePath += ".cmd"
		body = fmt.Sprintf("@echo off\r\ntype \"%s\"\r\ntype \"%s\" 1>&2\r\nexit /b %d\r\n", outPath, errPath, exitCode)
	}
	if err := os.WriteFile(probePath, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return probe.Prober{Path: probePath, Timeout: 5 * time.Second}
}

func TestRecoveredVideoDiagnosticsApplySubtitlePolicy(t *testing.T) {
	for _, test := range []struct {
		name, subtitle, sidecar   string
		valid, subtitles, unknown bool
	}{
		{"English", `,{"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"}}`, "", true, true, false},
		{"French", `,{"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"fra"}}`, "", false, true, false},
		{"ARIBUnknown", `,{"codec_type":"subtitle","codec_name":"arib_caption"}`, "", false, true, true},
		{"NoSubtitles", "", "", false, false, false},
		{"AudioMetadataIsNotASubtitle", `,{"codec_type":"audio","codec_name":"aac","tags":{"language":"eng","title":"English"}}`, "", false, false, false},
		{"EnglishSidecar", "", "fixture.en.srt", true, true, false},
		{"UnrelatedSidecar", "", "other.en.srt", false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSafetyFixture(t, "radarr")
			if test.sidecar != "" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(f.file.Path), test.sidecar), []byte("subtitle fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p := fixtureDiagnosticProber(t, `{"streams":[`+recoveredVideoJSON+test.subtitle+`]}`, recoveredVideoError+"\n"+recoveredVideoError, 0)
			v, err := p.Validate(t.Context(), f.file.Path)
			if err != nil {
				t.Fatal(err)
			}
			if v.Valid != test.valid || v.HasSubtitles != test.subtitles || v.HasUnknownLanguage != test.unknown {
				t.Fatalf("wrong policy result: %+v", v)
			}
			if !slices.Equal(v.ProbeWarnings, []string{"[mpeg2video] [error] Invalid frame dimensions 0x0."}) || v.FileInfo == nil || !v.SidecarsChecked {
				t.Fatal("missing warning or snapshots")
			}
			if test.unknown {
				if applyOldMediaGrace(v, time.Now().Year(), time.Now()).Valid || !applyOldMediaGrace(v, time.Now().Year()-11, time.Now()).Valid {
					t.Fatal("diagnostic changed the existing unidentified-language age policy")
				}
			}
		})
	}
}

func TestUnreliableProbeDiagnosticsStillBlock(t *testing.T) {
	goodJSON := `{"streams":[` + recoveredVideoJSON + `,{"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"}}]}`
	for _, test := range []struct {
		name, output, diagnostic string
		exitCode                 int
	}{
		{"SubtitleError", goodJSON, "[subrip @ 0x123] [error] Invalid subtitle packet", 0},
		{"PGSBitmapError", goodJSON, "[pgssub @ 0x123] [error] Bitmap dimensions (648x67) invalid.", 0},
		{"ContainerError", goodJSON, "[mpegts @ 0x123] [error] Packet corrupt", 0},
		{"MixedErrors", goodJSON, recoveredVideoError + "\n[arib_caption @ 0x456] [error] Invalid caption data", 0},
		{"UnknownDecoder", goodJSON, "[unknowncodec @ 0x123] [error] slice mismatch", 0},
		{"VideoCaptionError", goodJSON, "[mpeg2video @ 0x123] [error] Invalid A53 caption data", 0},
		{"MissingSeverity", goodJSON, strings.Replace(recoveredVideoError, "[error] ", "", 1), 0},
		{"WrongContext", goodJSON, strings.Replace(recoveredVideoError, "mpeg2video", "mpegts", 1), 0},
		{"ExitFailure", goodJSON, recoveredVideoError, 1},
		{"MalformedJSON", `{"streams":[`, recoveredVideoError, 0},
		{"MissingStreams", `{}`, recoveredVideoError, 0},
		{"NoRecoveryEvidence", `{"streams":[]}`, recoveredVideoError, 0},
		{"AmbiguousCodecType", `{"streams":[` + recoveredVideoJSON + `,{"codec_type":"subtitle","codec_name":"mpeg2video"}]}`, recoveredVideoError, 0},
		{"UnknownAfterManyAllowedLines", goodJSON, strings.Repeat(recoveredVideoError+"\n", 200) + "[mpegts @ 0x123] [error] Packet corrupt", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSafetyFixture(t, "radarr")
			p := fixtureDiagnosticProber(t, test.output, test.diagnostic, test.exitCode)
			f.service.probeFn = p.Validate
			// Even an English sidecar cannot override a genuinely failed probe.
			if err := os.WriteFile(filepath.Join(filepath.Dir(f.file.Path), "fixture.en.srt"), []byte("fixture subtitle"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := f.service.auditFile(t.Context(), f.client, f.file)
			if err == nil || !strings.Contains(err.Error(), "ffprobe diagnostics:") {
				t.Fatalf("lost failure/diagnostics: %v", err)
			}
			if len(f.mutations) != 0 || len(f.service.state.Pending()) != 0 || len(f.service.state.state.Attempts) != 0 {
				t.Fatal("unreliable probe mutated Arr or retry counters")
			}
			if strings.Contains(test.diagnostic, "Invalid caption data") && !strings.Contains(err.Error(), "Invalid caption data") {
				t.Fatal("actual subtitle diagnostic was lost")
			}
		})
	}
}

func TestRecoveredProbeCanCompleteLocalRemediation(t *testing.T) {
	for _, diagnostic := range nonSubtitleDiagnosticFixtures() {
		for _, kind := range []string{"sonarr", "radarr"} {
			for _, language := range []string{"eng", "fra", ""} {
				t.Run(diagnostic.name+"/"+kind+"/"+language, func(t *testing.T) {
					valid := language == "eng"
					f := newSafetyFixture(t, kind)
					subtitle := ""
					if language != "" {
						subtitle = `,{"codec_type":"subtitle","tags":{"language":"` + language + `"}}`
					}
					p := fixtureDiagnosticProber(t, `{"streams":[`+diagnostic.streams+subtitle+`],"format":{"format_name":"`+diagnostic.format+`"}}`, diagnostic.diagnostic, 0)
					f.service.probeFn = p.Validate
					keys := retryKeys(kind, f.file, []int{10, 12})
					for _, key := range keys {
						if _, err := f.service.state.Increment(key); err != nil {
							t.Fatal(err)
						}
					}
					if err := f.service.auditFile(t.Context(), f.client, f.file); err != nil {
						t.Fatal(err)
					}
					if valid {
						if len(f.mutations) != 0 {
							t.Fatal("accepted file mutated")
						}
						for _, key := range keys {
							if f.service.state.Attempts(key) != 0 {
								t.Fatal("valid file did not reset counters")
							}
						}
					} else if len(f.commands) != 1 || len(f.mutations) != 2 || len(f.service.state.Pending()) != 0 {
						t.Fatal("recovered video diagnostic blocked local remediation")
					}
					if _, err := os.Stat(f.file.Path); err != nil {
						t.Fatal("guard modified fixture media")
					}
				})
			}
		}
	}
}

func TestProbeOutputLimitStillBlocksRecoveredDiagnostics(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	p := fixtureDiagnosticProber(t, `{"streams":[`+recoveredVideoJSON+`]}`, strings.Repeat(recoveredVideoError+"\n", 2000), 0)
	if _, err := p.Validate(t.Context(), f.file.Path); err == nil {
		t.Fatal("oversized diagnostics were silently accepted as recovery")
	}
}

func TestRecoveredVideoCannotBypassChangedSubtitleSnapshot(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	p := fixtureDiagnosticProber(t, `{"streams":[`+recoveredVideoJSON+`]}`, recoveredVideoError, 0)
	v, err := p.Validate(t.Context(), f.file.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.file.Path), "fixture.en.srt"), []byte("new subtitle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.service.applyValidation(t.Context(), f.client, f.file, v, f.file.Path); err == nil || !strings.Contains(err.Error(), "matching subtitles changed") {
		t.Fatal("recovered diagnostic bypassed stale subtitle protection")
	}
	if len(f.mutations) != 0 || len(f.service.state.Pending()) != 0 {
		t.Fatal("stale subtitle decision changed Arr/state")
	}
}

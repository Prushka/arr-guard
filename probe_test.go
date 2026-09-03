package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNormalizeLanguage(t *testing.T) {
	tests := map[string]string{
		"eng":     "en",
		"en-US":   "en",
		"en_US":   "en",
		"English": "en",
		"jpn":     "jpn",
		"":        "",
	}
	for input, expected := range tests {
		if actual := normalizeLanguage(input); actual != expected {
			t.Errorf("normalizeLanguage(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func TestUnidentifiedLanguage(t *testing.T) {
	for _, language := range []string{"", "und", "unknown", "xyz"} {
		if !isUnidentifiedLanguage(language) {
			t.Errorf("language %q was not classified as unidentified", language)
		}
	}
	for _, language := range []string{"en", "eng", "jpn", "zxx", "mul"} {
		if isUnidentifiedLanguage(language) {
			t.Errorf("language %q was classified as unidentified", language)
		}
	}
}

func TestSubtitleStreamLanguageUsesEnglishTitleWhenTagIsUndetermined(t *testing.T) {
	if got := subtitleStreamLanguage(map[string]string{"language": "und", "title": "English SDH"}); got != "en" {
		t.Fatalf("subtitle language = %q, want en", got)
	}
}

func TestEnglishLabeledImageSubtitleCountsAsEnglish(t *testing.T) {
	for _, language := range []string{"en", "eng", "en-US"} {
		summary := newSubtitleSummary()
		summary.add(subtitleStreamLanguage(map[string]string{"language": language}))
		validation := summary.validation()
		if !validation.Valid || !validation.HasEnglish {
			t.Fatalf("image subtitle tagged %q was not accepted: %#v", language, validation)
		}
	}
}

func TestProberRecognizesEnglishSubtitleRegardlessOfCodecName(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "Movie.2024.mkv")
	if err := os.WriteFile(media, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(dir, "fake-ffprobe")
	probeBody := `{"streams":[{"codec_type":"subtitle","codec_name":"unlisted_future_subtitle_codec","tags":{"language":"eng"}}]}`
	if runtime.GOOS == "windows" {
		probePath += ".cmd"
		probeBody = "@echo off\r\necho " + probeBody + "\r\n"
	} else {
		probeBody = "#!/bin/sh\nprintf '%s' '" + probeBody + "'\n"
	}
	if err := os.WriteFile(probePath, []byte(probeBody), 0o700); err != nil {
		t.Fatal(err)
	}
	validation, err := (Prober{Path: probePath, Timeout: time.Second}).Validate(t.Context(), media)
	if err != nil {
		t.Fatal(err)
	}
	if !validation.Valid || !validation.HasEnglish || !validation.HasSubtitles {
		t.Fatalf("English PGS stream was not accepted: %#v", validation)
	}
}

func TestSidecarLanguage(t *testing.T) {
	tests := map[string]string{
		"en":         "en",
		"eng.forced": "en",
		"en-US.sdh":  "en",
		"jpn":        "jpn",
		"forced.sdh": "",
		"zz":         "",
	}
	for suffix, expected := range tests {
		if actual, _ := sidecarLanguage(suffix); actual != expected {
			t.Errorf("sidecarLanguage(%q) = %q, want %q", suffix, actual, expected)
		}
	}
}

func TestDiscoverExternalSubtitleFormats(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "Movie.2024.mkv")
	if err := os.WriteFile(media, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Movie.2024.en.srt", "Movie.2024.en.ass", "Movie.2024.en.vtt", "Movie.2024.en.sub", "Movie.2024.en.idx", "Movie.2024.en.sup", "Movie.2024.en.pgs"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "Movie.2024.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := discoverExternalSubtitles(media)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("discovered %d sidecars, want 7: %#v", len(got), got)
	}
	for _, subtitle := range got {
		if subtitle.Language != "en" {
			t.Fatalf("sidecar language = %q, want en", subtitle.Language)
		}
	}
}

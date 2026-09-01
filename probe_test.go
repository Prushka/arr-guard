package main

import (
	"os"
	"path/filepath"
	"testing"
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
	for _, name := range []string{"Movie.2024.en.srt", "Movie.2024.en.ass", "Movie.2024.en.vtt", "Movie.2024.en.sub", "Movie.2024.en.sup"} {
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
	if len(got) != 5 {
		t.Fatalf("discovered %d sidecars, want 5: %#v", len(got), got)
	}
	for _, subtitle := range got {
		if subtitle.Language != "en" {
			t.Fatalf("sidecar language = %q, want en", subtitle.Language)
		}
	}
}

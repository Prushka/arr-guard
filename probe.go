package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Prober struct {
	Path    string
	Timeout time.Duration
}

func (p Prober) Validate(ctx context.Context, filePath string) (Validation, error) {
	probeCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, p.Path,
		"-v", "error",
		// Only subtitle streams are relevant; avoiding audio/video metadata
		// keeps probes small without limiting subtitle discovery.
		"-select_streams", "s",
		"-show_entries", "stream=codec_type,codec_name:stream_tags=language,title:stream_disposition=default,forced,hearing_impaired",
		"-of", "json",
		filePath,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if probeCtx.Err() != nil {
			return Validation{}, fmt.Errorf("ffprobe timed out after %s", p.Timeout)
		}
		return Validation{}, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var result ProbeResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return Validation{}, fmt.Errorf("decode ffprobe output: %w", err)
	}

	languages := make(map[string]struct{})
	hasSubtitles := false
	hasEnglish := false
	hasUnknownLanguage := false
	for _, stream := range result.Streams {
		if stream.CodecType != "subtitle" {
			continue
		}
		hasSubtitles = true
		language := subtitleStreamLanguage(stream.Tags)
		if language == "" {
			language = "und"
		}
		if isUnidentifiedLanguage(language) {
			hasUnknownLanguage = true
		}
		languages[language] = struct{}{}
		if language == "en" {
			hasEnglish = true
		}
	}

	// Arr commonly stores subtitles as sidecar files next to the video. They
	// are not represented in the media-file API, so inspect matching files in
	// the same directory in addition to embedded ffprobe streams.
	external, err := discoverExternalSubtitles(filePath)
	if err != nil {
		return Validation{}, fmt.Errorf("inspect external subtitles: %w", err)
	}
	for _, subtitle := range external {
		hasSubtitles = true
		language := subtitle.Language
		if language == "" {
			language = "und"
		}
		if isUnidentifiedLanguage(language) {
			hasUnknownLanguage = true
		}
		languages[language] = struct{}{}
		if language == "en" {
			hasEnglish = true
		}
	}

	values := make([]string, 0, len(languages))
	for language := range languages {
		values = append(values, language)
	}
	sort.Strings(values)
	validation := Validation{
		Valid:              hasSubtitles && hasEnglish,
		HasSubtitles:       hasSubtitles,
		HasEnglish:         hasEnglish,
		HasUnknownLanguage: hasUnknownLanguage,
		SubtitleLangs:      values,
	}
	if !hasSubtitles {
		validation.Reason = "no embedded or sidecar subtitle"
	} else if !hasEnglish {
		validation.Reason = "no English subtitle stream or sidecar"
	}
	return validation, nil
}

type externalSubtitle struct {
	Language string
}

var subtitleExtensions = map[string]struct{}{
	".ass":  {},
	".dfxp": {},
	".idx":  {},
	".mks":  {},
	".mpl2": {},
	".sami": {},
	".smi":  {},
	".scc":  {},
	".ssa":  {},
	".srt":  {},
	".sub":  {},
	".sup":  {},
	".stl":  {},
	".ttml": {},
	".usf":  {},
	".vtt":  {},
}

func discoverExternalSubtitles(filePath string) ([]externalSubtitle, error) {
	directory := filepath.Dir(filePath)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(filePath)
	extension := filepath.Ext(name)
	if extension == "" {
		return nil, nil
	}
	stem := strings.TrimSuffix(name, extension)
	prefix := strings.ToLower(stem) + "."
	result := make([]externalSubtitle, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		candidate := entry.Name()
		lowerCandidate := strings.ToLower(candidate)
		if !strings.HasPrefix(lowerCandidate, prefix) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(candidate))
		if _, ok := subtitleExtensions[ext]; !ok {
			continue
		}
		suffix := strings.TrimSuffix(candidate[len(stem)+1:], filepath.Ext(candidate))
		language, _ := sidecarLanguage(suffix)
		result = append(result, externalSubtitle{Language: language})
	}
	return result, nil
}

// sidecarLanguage returns a language code when the filename contains one and
// reports whether it found a recognized non-language token. Tokens such as
// "forced" and "sdh" are intentionally treated as unidentified rather than
// pretending they identify a language.
func sidecarLanguage(suffix string) (string, bool) {
	parts := strings.FieldsFunc(strings.ToLower(suffix), func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	})
	known := false
	for i, part := range parts {
		candidate := part
		if i+1 < len(parts) && len(part) == 2 && len(parts[i+1]) == 2 {
			candidate = part + "-" + parts[i+1]
		}
		language := normalizeLanguage(candidate)
		if language == "en" {
			return "en", true
		}
		if isKnownLanguage(language) {
			known = true
		}
	}
	if known {
		for _, part := range parts {
			language := normalizeLanguage(part)
			if isKnownLanguage(language) && language != "en" {
				return language, true
			}
		}
	}
	return "", false
}

func isUnknownLanguage(language string) bool {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "", "und", "unknown", "unk", "undefined":
		return true
	default:
		return false
	}
}

func subtitleStreamLanguage(tags map[string]string) string {
	language := normalizeLanguage(tags["language"])
	if language != "" && !isUnidentifiedLanguage(language) {
		return language
	}
	if titleLanguage := languageFromLabel(tags["title"]); titleLanguage != "" {
		return titleLanguage
	}
	return language
}

func languageFromLabel(value string) string {
	parts := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == ' ' || r == '(' || r == ')' || r == '[' || r == ']'
	})
	for _, part := range parts {
		language := normalizeLanguage(part)
		if language == "en" {
			return "en"
		}
		if isKnownLanguage(language) && language != "en" {
			return language
		}
	}
	return ""
}

func isUnidentifiedLanguage(language string) bool {
	return isUnknownLanguage(language) || !isKnownLanguage(language)
}

func isKnownLanguage(language string) bool {
	_, ok := knownLanguageCodes[language]
	return ok
}

// This covers the ISO-639 codes most often used in Arr subtitle filenames.
// Unrecognized codes remain unidentified, which is important for the old
// media grace rule instead of incorrectly treating them as a known language.
var knownLanguageCodes = func() map[string]struct{} {
	values := strings.Fields("ab af ara ar az ba be bg ben bn bs ca chi zh zho cs cze ces cy da dan de deu ger el ell gre en eng es spa et eu fa fas per fi fin fil fr fra fre gl he heb hi hin hr hrv hu hun id ind is isl ice it ita ja jpn ka kk km kn ko kor lt lv mk ml mn mr ms msa mt nb nl nld dut nn no nor pl pol pt por ro ron rum ru rus sk slk slo slv sq srp sr sv swe ta tam te tel th tha tr tur uk ukr ur uz vi vie mul zxx")
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}()

func normalizeLanguage(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	base, _, _ := strings.Cut(value, "-")
	switch base {
	case "en", "eng", "english":
		return "en"
	default:
		return base
	}
}

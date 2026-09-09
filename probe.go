package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	if !filepath.IsAbs(filePath) {
		return Validation{}, errors.New("probe requires an absolute local file path")
	}
	before, err := os.Stat(filePath)
	if err != nil {
		return Validation{}, err
	}
	if !before.Mode().IsRegular() || before.Size() == 0 {
		return Validation{}, errors.New("probe requires a nonempty regular file")
	}
	directory, err := os.Stat(filepath.Dir(filePath))
	if err != nil {
		return Validation{}, err
	}
	if p.Timeout <= 0 {
		p.Timeout = 10 * time.Minute
	}
	probeCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, p.Path,
		"-v", "error",
		"-protocol_whitelist", "file,pipe,crypto",
		// Only subtitle streams are relevant; avoiding audio/video metadata
		// keeps probes small without limiting subtitle discovery.
		"-select_streams", "s",
		"-show_entries", "stream=codec_type,codec_name:stream_tags=language,title:stream_disposition=default,forced,hearing_impaired",
		"-of", "json",
		"-i", filePath,
	)
	stdout := limitedBuffer{limit: 4 << 20}
	stderr := limitedBuffer{limit: 64 << 10}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if probeCtx.Err() != nil {
			return Validation{}, fmt.Errorf("ffprobe interrupted: %w", probeCtx.Err())
		}
		return Validation{}, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stderr.Len() > 0 {
		return Validation{}, errors.New("ffprobe reported media errors; subtitle absence is not trustworthy")
	}

	var result ProbeResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return Validation{}, fmt.Errorf("decode ffprobe output: %w", err)
	}
	if result.Streams == nil {
		return Validation{}, errors.New("ffprobe output is missing the streams array")
	}

	summary := newSubtitleSummary()
	for _, stream := range result.Streams {
		if stream.CodecType != "subtitle" {
			continue
		}
		summary.add(subtitleStreamLanguage(stream.Tags))
	}

	// Arr commonly stores subtitles as sidecar files next to the video. They
	// are not represented in the media-file API, so inspect matching files in
	// the same directory in addition to embedded ffprobe streams.
	external, err := discoverExternalSubtitles(filePath)
	if err != nil {
		return Validation{}, fmt.Errorf("inspect external subtitles: %w", err)
	}
	for _, subtitle := range external {
		summary.add(subtitle.Language)
	}

	after, err := os.Stat(filePath)
	if err != nil || !sameDiskFile(before, after) {
		return Validation{}, errors.New("media changed during probe")
	}
	validation := summary.validation()
	validation.fileInfo = after
	validation.dirInfo = directory
	validation.sidecars = external
	validation.sidecarsChecked = true
	if err := validation.checkSubtitleSnapshot(filePath); err != nil {
		return Validation{}, err
	}
	return validation, nil
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Len() int       { return b.buffer.Len() }
func (b *limitedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *limitedBuffer) String() string { return b.buffer.String() }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("ffprobe output exceeds safety limit")
	}
	return b.buffer.Write(p)
}

// subtitleSummary records the stream type from ffprobe rather than a fixed
// codec allowlist. This includes text, bitmap, and future subtitle codecs as
// long as ffprobe reports codec_type=subtitle.
type subtitleSummary struct {
	languages  map[string]struct{}
	hasEnglish bool
	hasUnknown bool
}

func newSubtitleSummary() subtitleSummary {
	return subtitleSummary{languages: make(map[string]struct{})}
}

func (s *subtitleSummary) add(language string) {
	if language == "" {
		language = "und"
	}
	if isUnidentifiedLanguage(language) {
		s.hasUnknown = true
	}
	s.languages[language] = struct{}{}
	if language == "en" {
		s.hasEnglish = true
	}
}

func (s subtitleSummary) validation() Validation {
	values := make([]string, 0, len(s.languages))
	for language := range s.languages {
		values = append(values, language)
	}
	sort.Strings(values)
	validation := Validation{
		Valid:              len(s.languages) > 0 && s.hasEnglish,
		HasSubtitles:       len(s.languages) > 0,
		HasEnglish:         s.hasEnglish,
		HasUnknownLanguage: s.hasUnknown,
		SubtitleLangs:      values,
	}
	if !validation.HasSubtitles {
		validation.Reason = "no embedded or sidecar subtitle"
	} else if !validation.HasEnglish {
		validation.Reason = "no English subtitle stream or sidecar"
	}
	return validation
}

type externalSubtitle struct {
	Language string
	Name     string
	Info     os.FileInfo
}

// Directory identity must stay stable, but unrelated entries and directory mtime
// do not affect this media's subtitle decision. Compare the matching files instead.
func (v Validation) checkSubtitleSnapshot(mediaPath string) error {
	dir, err := os.Stat(filepath.Dir(mediaPath))
	if err != nil || v.dirInfo == nil || !os.SameFile(v.dirInfo, dir) {
		return deferProcessing(errors.New("subtitle directory changed or is unavailable"))
	}
	if !v.sidecarsChecked {
		return errors.New("matching subtitles have no verified snapshot")
	}
	current, err := discoverExternalSubtitles(mediaPath)
	if err != nil {
		return deferProcessing(fmt.Errorf("recheck matching subtitles: %w", err))
	}
	if len(current) != len(v.sidecars) {
		return deferProcessing(errors.New("matching subtitles changed since probe"))
	}
	for i, subtitle := range current {
		previous := v.sidecars[i]
		if subtitle.Name != previous.Name || !sameDiskFile(subtitle.Info, previous.Info) {
			return deferProcessing(errors.New("matching subtitles changed since probe"))
		}
	}
	return nil
}

var subtitleExtensions = map[string]struct{}{
	".ass":  {},
	".dfxp": {},
	".idx":  {},
	".mks":  {},
	".mpl2": {},
	".pgs":  {},
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
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, errors.New("matching subtitle is empty or not a regular file; retry after import completes")
		}
		// Lowercasing Unicode can change byte lengths. Slice the same normalized
		// string that was prefix-matched, not by the original media stem length.
		suffix := strings.TrimSuffix(lowerCandidate[len(prefix):], ext)
		language, _ := sidecarLanguage(suffix)
		result = append(result, externalSubtitle{Language: language, Name: candidate, Info: info})
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
	metadata := func(key string) string {
		for name, value := range tags {
			if strings.EqualFold(name, key) {
				return value
			}
		}
		return ""
	}
	language := normalizeLanguage(metadata("language"))
	if language != "" && !isUnidentifiedLanguage(language) {
		return language
	}
	if titleLanguage := languageFromLabel(metadata("title")); titleLanguage != "" {
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

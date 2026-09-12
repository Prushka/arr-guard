package probe

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
)

// Context and severity must be present on each line; do not inherit a previous
// decoder's context for an unclassified continuation or global discovery error.
var probeDiagnostic = regexp.MustCompile(`^\[([a-zA-Z0-9_,.-]+)\] \[(error|fatal)\] (.+)$`)

// avpriv_new_chapter rejects only this chapter entry, not the stream inventory.
var chapterTimestampDiagnostic = regexp.MustCompile(`^Chapter end time -?[0-9]+ before start -?[0-9]+$`)

var probeLogAddress = regexp.MustCompile(` @ (?:0x)?[0-9a-fA-F]+\]`)

func classifyProbeDiagnostics(raw string, result ProbeResult) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	failure := func() ([]string, error) {
		return nil, withProbeDiagnostics(errors.New("ffprobe reported media errors; subtitle discovery is not trustworthy"), raw)
	}
	// Match decoder names to ffprobe's own stream types. All streams with a
	// matching name must be audio/video; an unknown or subtitle type wins.
	decoders := map[string]bool{}
	for _, stream := range result.Streams {
		allowed := stream.CodecType == "video" || stream.CodecType == "audio"
		if previous, exists := decoders[stream.CodecName]; exists {
			allowed = allowed && previous
		}
		decoders[stream.CodecName] = allowed
	}
	formats := strings.Split(result.Format.Name, ",")
	formats = append(formats, result.Format.Name)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := probeDiagnostic.FindStringSubmatch(probeLogAddress.ReplaceAllString(line, "]"))
		if parts == nil || subtitleRelatedDiagnostic(parts[3]) {
			return failure()
		}
		context, severity, message := parts[1], parts[2], parts[3]
		isFormat := false
		for _, format := range formats {
			isFormat = isFormat || context == format
		}
		if isFormat {
			if severity == "error" && chapterTimestampDiagnostic.MatchString(message) {
				continue
			}
			return failure()
		}
		if !decoders[context] {
			return failure()
		}
	}
	return probeDiagnosticLines(raw), nil
}

func subtitleRelatedDiagnostic(message string) bool {
	message = strings.ToLower(message)
	// Video decoders can also parse embedded caption data. Their context alone
	// cannot establish that these messages are unrelated to subtitle discovery.
	for _, marker := range []string{"subtitle", "caption", "a53", "a/53", "cea-608", "cea-708", "eia-608", "eia-708", "cc_data", "cc_count", "cc_type", "teletext", "user data", "user_data"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// Retain the real diagnostic while keeping repetitive or hostile stderr from
// flooding logs. Classification above always inspects the full bounded stderr.
func withProbeDiagnostics(cause error, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return cause
	}
	return fmt.Errorf("%w; ffprobe diagnostics: %s", cause, strings.Join(probeDiagnosticLines(raw), " | "))
}

// Both accepted diagnostics and failures share the same total log budget.
// Classification must finish on the full bounded stderr before calling this.
func probeDiagnosticLines(raw string) []string {
	seen := map[string]bool{}
	var lines []string
	characters := 0
	const maxDiagnosticCharacters = 2048
	for _, line := range strings.Split(raw, "\n") {
		line = probeLogAddress.ReplaceAllString(strings.TrimSpace(line), "]")
		line = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, line)
		if line != "" && !seen[line] {
			seen[line] = true
			detail := []rune(line)
			if len(lines) > 0 {
				characters += 3 // The separator used by withProbeDiagnostics.
			}
			remaining := max(0, maxDiagnosticCharacters-characters)
			if len(detail) > remaining {
				lines = append(lines, string(detail[:remaining])+" [truncated]")
				break
			}
			characters += len(detail)
			lines = append(lines, line)
		}
	}
	return lines
}

func probeEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		// FFREPORT can otherwise enable log-file writes behind a read-only probe.
		if !strings.EqualFold(name, "FFREPORT") && !strings.EqualFold(name, "AV_LOG_FORCE_COLOR") && !strings.EqualFold(name, "AV_LOG_FORCE_NOCOLOR") {
			env = append(env, entry)
		}
	}
	return append(env, "AV_LOG_FORCE_NOCOLOR=1")
}

package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
)

// With repeat+level enabled, every line carries its own context and severity.
// Match only the observed MPEG-2 startup error, not arbitrary decoder failures.
var recoveredMPEG2Dimensions = regexp.MustCompile(`^\[mpeg2video @ (?:0x)?[0-9a-fA-F]+\] \[error\] Invalid frame dimensions 0x0\.$`)
var probeLogAddress = regexp.MustCompile(` @ (?:0x)?[0-9a-fA-F]+\]`)

const recoveredMPEG2Warning = "[mpeg2video] [error] Invalid frame dimensions 0x0."

func classifyProbeDiagnostics(raw string, streams []ProbeStream) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	failure := func() ([]string, error) {
		return nil, withProbeDiagnostics(errors.New("ffprobe reported media errors; subtitle discovery is not trustworthy"), raw)
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !recoveredMPEG2Dimensions.MatchString(line) {
			return failure()
		}
	}
	// The log context does not identify a stream index. Require every MPEG-2
	// video stream to have recovered dimensions, so another stream cannot mask it.
	found := false
	for _, stream := range streams {
		if stream.CodecType == "video" && stream.CodecName == "mpeg2video" {
			if stream.Width <= 0 || stream.Height <= 0 {
				return failure()
			}
			found = true
		}
	}
	if !found {
		return failure()
	}
	return []string{recoveredMPEG2Warning}, nil
}

// Retain the real diagnostic while keeping repetitive or hostile stderr from
// flooding logs. Classification above always inspects the full bounded stderr.
func withProbeDiagnostics(cause error, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return cause
	}
	seen := map[string]bool{}
	var lines []string
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
			lines = append(lines, line)
		}
	}
	detail := []rune(strings.Join(lines, " | "))
	const maxDiagnosticCharacters = 2048
	if len(detail) > maxDiagnosticCharacters {
		detail = append(detail[:maxDiagnosticCharacters], []rune(" [truncated]")...)
	}
	return fmt.Errorf("%w; ffprobe diagnostics: %s", cause, string(detail))
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

package probe

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestProbeDiagnosticsUseStreamTypes(t *testing.T) {
	result := ProbeResult{Streams: []ProbeStream{
		{CodecType: "video", CodecName: "mpeg2video"}, // Dimensions are irrelevant.
		{CodecType: "video", CodecName: "mjpeg", Disposition: map[string]int{"attached_pic": 1}},
		{CodecType: "audio", CodecName: "aac"},
		{CodecType: "subtitle", CodecName: "subrip", Tags: map[string]string{"language": "eng"}},
	}}
	result.Format.Name = "matroska,webm"
	video := "[mpeg2video @ 0x123] [error] slice mismatch"
	chapter := "[matroska,webm @ 0x123] [error] Chapter end time 0 before start 40040000000"
	jpeg := "[mjpeg @ 0x123] [error] dqt: invalid precision\n[mjpeg @ 0x123] [error] unable to decode APP fields: Invalid data found when processing input\n[mjpeg @ 0x123] [fatal] No JPEG data found in image"
	for _, test := range []struct {
		name, diagnostic string
		blocked          bool
	}{
		{"VideoDecode", video, false},
		{"VideoFatal", strings.Replace(video, "[error]", "[fatal]", 1), false},
		{"VideoZeroDimensions", "[mpeg2video @ 0x123] [error] Invalid frame dimensions 0x0.", false},
		{"JPEGAttachment", jpeg, false},
		{"AudioDecode", "[aac @ 0x123] [error] Error decoding AAC frame header.", false},
		{"ChapterTimestamps", chapter, false},
		{"AllNonSubtitle", video + "\n" + jpeg + "\n" + chapter, false},
		{"Subtitle", "[subrip @ 0x123] [error] Invalid subtitle packet", true},
		{"SubtitleGenericError", "[subrip @ 0x123] [error] Invalid data", true},
		{"CaptionInVideo", "[mpeg2video @ 0x123] [error] Invalid closed caption data", true},
		{"A53InVideo", "[mpeg2video @ 0x123] [error] Invalid A53 side data", true},
		{"CaptionUserData", "[mpeg2video @ 0x123] [error] Invalid user_data payload", true},
		{"UnknownDecoder", "[unknowncodec @ 0x123] [error] broken data", true},
		{"Container", "[matroska,webm @ 0x123] [error] File ended prematurely", true},
		{"ChapterTrailingError", chapter + "; failed to parse tracks", true},
		{"ChapterUnknownContext", strings.Replace(chapter, "matroska,webm", "unknown", 1), true},
		{"ChapterFatal", strings.Replace(chapter, "[error]", "[fatal]", 1), true},
		{"MissingContext", "[error] Could not find codec parameters", true},
		{"MissingSeverity", "[mjpeg @ 0x123] No JPEG data found in image", true},
		{"Panic", strings.Replace(video, "[error]", "[panic]", 1), true},
		{"UnclassifiedContinuation", video + "\nfailed to discover streams", true},
		{"SubtitleAfterIgnored", jpeg + "\n[subrip @ 0x123] [error] Invalid subtitle packet", true},
		{"LateDiscoveryFailure", strings.Repeat(video+"\n", 200) + "[error] Could not find codec parameters", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			warnings, err := classifyProbeDiagnostics(test.diagnostic, result)
			if (err != nil) != test.blocked {
				t.Fatalf("diagnostic error = %v, want blocked = %t", err, test.blocked)
			}
			if test.blocked {
				if len(warnings) != 0 || !strings.Contains(err.Error(), "ffprobe diagnostics:") {
					t.Fatal("failed discovery lost its diagnostics or became a warning")
				}
			} else if len(warnings) == 0 || strings.Contains(strings.Join(warnings, ""), "0x123") {
				t.Fatal("missing or unstable ignored diagnostics")
			}
		})
	}
	for _, streamType := range []string{"subtitle", "data", "attachment", ""} {
		ambiguous := result
		ambiguous.Streams = append(slices.Clone(result.Streams), ProbeStream{CodecName: "mjpeg", CodecType: streamType})
		if _, err := classifyProbeDiagnostics(jpeg, ambiguous); err == nil {
			t.Fatalf("ambiguous codec type %q accepted", streamType)
		}
	}
	result.Format.Name = "mpeg2video"
	if _, err := classifyProbeDiagnostics(video, result); err == nil {
		t.Fatal("format/decoder context collision accepted")
	}
}

func TestIgnoredProbeDiagnosticFormattingIsBounded(t *testing.T) {
	raw := "[mjpeg @ 0x123] [error] dqt: invalid precision\n[mjpeg @ 0x456] [error] dqt: invalid precision"
	result := ProbeResult{Streams: []ProbeStream{{CodecType: "video", CodecName: "mjpeg"}}}
	warnings, err := classifyProbeDiagnostics(raw, result)
	if err != nil || !slices.Equal(warnings, []string{"[mjpeg] [error] dqt: invalid precision"}) {
		t.Fatalf("wrong diagnostic normalization: %v %v", warnings, err)
	}
	raw += "\n[mjpeg @ 0x123] [error] " + strings.Repeat("broken ", 1000)
	warnings, err = classifyProbeDiagnostics(raw, result)
	text := strings.Join(warnings, " | ")
	if err != nil || len(text) > 2100 || !strings.Contains(text, "[truncated]") {
		t.Fatal("ignored diagnostics exceeded their log budget")
	}
}

func TestProbeDiagnosticFormattingPreservesCauseAndBounds(t *testing.T) {
	err := withProbeDiagnostics(context.DeadlineExceeded, "[mpeg2video @ 000001A1234] [error] Invalid frame dimensions 0x0."+"\n"+"[mpeg2video @ 000001A1234] [error] Invalid frame dimensions 0x0."+"\n[codec @ 0xabc] [error] broken\x1b\rmessage")
	if !errors.Is(err, context.DeadlineExceeded) || strings.Count(err.Error(), "Invalid frame dimensions 0x0.") != 1 || strings.ContainsAny(err.Error(), "\x1b\r\n") {
		t.Fatal("diagnostic formatting lost cause, dimensions, or log bounds", err)
	}
	if strings.Contains(err.Error(), "000001A1234") || strings.Contains(err.Error(), "0xabc") {
		t.Fatal("unstable log pointer retained")
	}
	err = withProbeDiagnostics(errors.New("failed"), strings.Repeat("failure", 1000))
	if !strings.Contains(err.Error(), "[truncated]") || len(err.Error()) > 2200 {
		t.Fatal("diagnostic log is unbounded")
	}
}

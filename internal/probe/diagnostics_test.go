package probe

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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

package probe

import (
	"strings"
	"testing"
)

func TestProbeEnvironmentDisablesImplicitReportsAndColors(t *testing.T) {
	t.Setenv("FFREPORT", "file=should-not-exist.log")
	t.Setenv("AV_LOG_FORCE_COLOR", "1")
	t.Setenv("AV_LOG_FORCE_NOCOLOR", "0")
	for _, entry := range probeEnvironment() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, "FFREPORT") || strings.EqualFold(name, "AV_LOG_FORCE_COLOR") {
			t.Fatal("probe can create reports or force ANSI logs")
		}
		if strings.EqualFold(name, "AV_LOG_FORCE_NOCOLOR") && value != "1" {
			t.Fatal("non-deterministic color setting")
		}
	}
}

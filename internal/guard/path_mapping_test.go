package guard

import (
	"strings"
	"testing"

	"github.com/Prushka/arr-guard/internal/config"
)

func TestUnicodeMappingPrefix(t *testing.T) {
	s := &Service{config: config.Config{PathMappings: []config.PathMapping{{From: "C:/" + strings.Repeat("\u0130", 20), To: "/mapped"}}}}
	if got := s.mapPath("C:/" + strings.Repeat("i", 20) + "/film.mkv"); got != "/mapped/film.mkv" {
		t.Fatal("Unicode mapping suffix was sliced incorrectly")
	}
}

package probe

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestValidationJSONExcludesFilesystemSnapshots(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v := Validation{
		FileInfo: info, DirectoryInfo: info, SidecarsChecked: true,
		Sidecars: []ExternalSubtitle{{Name: "private-sidecar.srt", Info: info}},
		Valid:    true, HasSubtitles: true, HasEnglish: true,
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"FileInfo", "DirectoryInfo", "Sidecars", "SidecarsChecked", "private-sidecar"} {
		if strings.Contains(string(data), name) {
			t.Fatalf("validation JSON exposed snapshot field %q", name)
		}
	}
	var decoded Validation
	if err := json.Unmarshal(data, &decoded); err != nil || !decoded.Valid || !decoded.HasEnglish || decoded.FileInfo != nil || decoded.SidecarsChecked {
		t.Fatal("validation JSON changed policy fields or trusted an unverified snapshot")
	}
}

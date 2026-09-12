package arr

import "testing"

func TestUnknownEpisodeReleaseYearPreventsAgeSkip(t *testing.T) {
	if year := latestEpisodeReleaseYearByFile([]Episode{{ID: 1, EpisodeFileID: 3, AirDate: "1900-01-01"}, {ID: 2, EpisodeFileID: 3}})[3]; year != 0 {
		t.Fatal("unknown episode date caused age skip")
	}
}

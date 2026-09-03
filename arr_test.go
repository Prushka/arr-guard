package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestArrClientSearchCommands(t *testing.T) {
	tests := []struct {
		kind       string
		episodeIDs []int
		want       CommandRequest
	}{
		{kind: "sonarr", want: CommandRequest{Name: "SeriesSearch", SeriesID: 42}},
		{kind: "sonarr", episodeIDs: []int{10, 11}, want: CommandRequest{Name: "EpisodeSearch", EpisodeIDs: []int{10, 11}}},
		{kind: "radarr", want: CommandRequest{Name: "MoviesSearch", MovieIDs: []int{42}}},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v3/command" {
					t.Fatalf("request = %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("X-Api-Key"); got != "secret" {
					t.Fatalf("API key header = %q", got)
				}
				var got CommandRequest
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				if got.Name != test.want.Name || got.SeriesID != test.want.SeriesID || !equalInts(got.MovieIDs, test.want.MovieIDs) || !equalInts(got.EpisodeIDs, test.want.EpisodeIDs) {
					t.Fatalf("command = %#v, want %#v", got, test.want)
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()

			client := testArrClient(test.kind, server.URL)
			var err error
			if test.kind == "sonarr" && len(test.episodeIDs) == 0 {
				err = client.SearchSeries(context.Background(), 42)
			} else {
				err = client.SearchEpisodes(context.Background(), 42, test.episodeIDs)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArrClientEpisodeSearchRequiresIDs(t *testing.T) {
	client := testArrClient("sonarr", "http://arr.invalid")
	if err := client.SearchEpisodes(context.Background(), 42, nil); err == nil {
		t.Fatal("expected missing episode IDs to be rejected")
	}
}

func TestArrClientEpisodeIDsForFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v3/episode" || r.URL.Query().Get("seriesId") != "42" {
			t.Fatalf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[{"id":10,"episodeFileId":7},{"id":11,"episodeFileId":8},{"id":12,"episodeFileId":7}]`))
	}))
	defer server.Close()
	client := testArrClient("sonarr", server.URL)
	got, err := client.EpisodeIDsForFile(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(got, []int{10, 12}) {
		t.Fatalf("episode IDs = %#v", got)
	}
}

func TestArrClientListSonarrFilesUsesLatestEpisodeReleaseYear(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/series":
			_, _ = w.Write([]byte(`[{"id":42,"year":1920,"path":"/media/Show"}]`))
		case "/api/v3/episodefile":
			if r.URL.Query().Get("seriesId") != "42" {
				t.Fatalf("episode file query = %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"id":7,"seriesId":42,"relativePath":"S01E01-E02.mkv"}]`))
		case "/api/v3/episode":
			if r.URL.Query().Get("seriesId") != "42" {
				t.Fatalf("episode query = %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"id":10,"episodeFileId":7,"airDate":"1920-01-01"},{"id":11,"episodeFileId":7,"airDate":"1980-01-01"}]`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	files, err := testArrClient("sonarr", server.URL).ListSubtitleGuardFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Year != 1980 {
		t.Fatalf("Sonarr file years = %#v, want latest episode year 1980", files)
	}
}

func TestArrClientRemediationRequests(t *testing.T) {
	type request struct {
		method string
		path   string
		query  url.Values
	}
	requests := make(chan request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests <- request{method: r.Method, path: r.URL.Path, query: r.URL.Query()}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := testArrClient("radarr", server.URL)

	if err := client.DeleteMediaFile(context.Background(), 17); err != nil {
		t.Fatal(err)
	}
	if err := client.FailQueueItem(context.Background(), 23, "bad subtitles"); err != nil {
		t.Fatal(err)
	}

	deleted := <-requests
	if deleted.method != http.MethodDelete || deleted.path != "/api/v3/moviefile/17" {
		t.Fatalf("delete request = %#v", deleted)
	}
	failed := <-requests
	if failed.method != http.MethodDelete || failed.path != "/api/v3/queue/23" || failed.query.Get("blocklist") != "true" || failed.query.Get("removeFromClient") != "true" || failed.query.Get("skipRedownload") != "true" {
		t.Fatalf("queue failure request = %#v", failed)
	}
}

func testArrClient(kind, baseURL string) *ArrClient {
	return NewArrClient(ArrConfig{Name: kind, Kind: kind, URL: baseURL, APIKey: "secret", APIVersion: "v3"}, slog.Default())
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

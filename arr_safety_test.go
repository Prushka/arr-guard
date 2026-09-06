package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLiveErrorCategoriesDoNotExposeDiagnosticValues(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("private diagnostic: %w", context.DeadlineExceeded), "deadline exceeded"},
		{fmt.Errorf("private diagnostic: %w", context.Canceled), "canceled"},
		{fmt.Errorf("private diagnostic: %w", &ArrHTTPError{Status: 503}), "HTTP 503"},
		{&url.Error{Op: "GET", URL: "https://example.invalid/private", Err: os.ErrDeadlineExceeded}, "network timeout"},
		{fmt.Errorf("private diagnostic"), "other read/probe failure (*errors.errorString)"},
	} {
		if got := liveErrorCategory(test.err); got != test.want {
			t.Errorf("category = %q, want %q", got, test.want)
		}
	}
}

func TestArrReadOnlyClientBlocksEveryMutation(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	c := testArrClient("radarr", server.URL)
	c.readOnly = true
	for _, action := range []func() error{
		func() error { return c.DeleteMediaFile(t.Context(), 17) }, func() error { return c.FailQueueItem(t.Context(), 7, "reason") }, func() error { return c.MarkHistoryFailed(t.Context(), 9) }, func() error { return c.SearchEpisodes(t.Context(), 3, nil) },
	} {
		if err := action(); err == nil {
			t.Fatal("mutation permitted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("read-only client reached mutation server")
	}
}

func TestArrRedirectNeverForwardsAPIKey(t *testing.T) {
	var forwarded atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); w.WriteHeader(204) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	if err := testArrClient("sonarr", origin.URL).Test(t.Context()); err == nil {
		t.Fatal("redirect accepted")
	}
	if forwarded.Load() != 0 {
		t.Fatal("API key sent to redirect target")
	}
}

func TestArrRejectsIncompleteResourceResponses(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"id":18,"movieId":3}`, `{"id":17,"movieId":0}`, `{"id":17,"movieId":3} {}`, strings.Repeat(" ", 16<<20) + `{}`} {
		t.Run(fmt.Sprintf("length%d", len(body)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			if _, err := testArrClient("radarr", server.URL).GetMediaFile(t.Context(), 17); err == nil {
				t.Fatal("invalid resource accepted")
			}
		})
	}
}

func TestArrPaginationAndRepeatedPageRejection(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		t.Run(fmt.Sprint(repeated), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Query().Get("page") == "1" || repeated {
					_, _ = w.Write([]byte(`{"records":[{"id":1}],"totalRecords":2}`))
				} else {
					_, _ = w.Write([]byte(`{"records":[{"id":2}],"totalRecords":2}`))
				}
			}))
			defer server.Close()
			c := testArrClient("radarr", server.URL)
			queue, err := c.Queue(t.Context())
			if repeated {
				if err == nil {
					t.Fatal("repeated page accepted")
				}
			} else if err != nil || len(queue) != 2 {
				t.Fatal("pagination failed", err)
			}
			if calls.Load() != 2 {
				t.Fatal("unexpected pagination count")
			}
			history, err := c.DownloadHistory(t.Context(), "download")
			if repeated {
				if err == nil {
					t.Fatal("repeated history page accepted")
				}
			} else if err != nil || len(history) != 2 {
				t.Fatal("history pagination failed", err)
			}
		})
	}
}

func TestLiveTransportRejectsUnapprovedPathsAndMethods(t *testing.T) {
	base, err := url.Parse("http://127.0.0.1:1/base")
	if err != nil {
		t.Fatal(err)
	}
	rt := &liveReadTransport{base: base}
	for _, test := range []struct{ method, path string }{{"DELETE", "/base/api/v3/moviefile/1"}, {"POST", "/base/api/v3/command"}, {"GET", "/base/api/v3/history/failed/1"}, {"GET", "/base/api/v3/config/host"}, {"GET", "/movie"}, {"GET", "/base/api/v3/movie/-1"}} {
		r, err := http.NewRequestWithContext(context.Background(), test.method, "http://127.0.0.1:1"+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rt.RoundTrip(r); err == nil || !strings.Contains(err.Error(), "safety transport blocked") {
			t.Fatal("unsafe live request escaped the gate")
		}
	}
	if rt.reads.Load() != 0 {
		t.Fatal("blocked requests reached network")
	}
}

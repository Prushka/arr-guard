package guard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Prushka/arr-guard/internal/arr"
)

func TestLiveErrorCategoriesDoNotExposeDiagnosticValues(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("private diagnostic: %w", context.DeadlineExceeded), "deadline exceeded"},
		{fmt.Errorf("private diagnostic: %w", context.Canceled), "canceled"},
		{fmt.Errorf("private diagnostic: %w", &arr.HTTPError{Status: 503}), "HTTP 503"},
		{&url.Error{Op: "GET", URL: "https://example.invalid/private", Err: os.ErrDeadlineExceeded}, "network timeout"},
		{fmt.Errorf("private diagnostic"), "other read/probe failure (*errors.errorString)"},
	} {
		if got := liveErrorCategory(test.err); got != test.want {
			t.Errorf("category = %q, want %q", got, test.want)
		}
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

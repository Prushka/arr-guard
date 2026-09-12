package guard

import (
	"log/slog"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/config"
)

func testArrClient(kind, baseURL string) *arr.Client {
	return arr.NewClient(config.Arr{Name: kind, Kind: kind, URL: baseURL, APIKey: "secret", APIVersion: "v3"}, slog.Default())
}

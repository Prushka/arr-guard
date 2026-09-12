package arr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type HTTPError struct {
	Method string
	Status int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Arr %s returned HTTP %d", e.Method, e.Status)
}

func CanonicalIDs(ids []int) []int {
	ids = append([]int(nil), ids...)
	slices.Sort(ids)
	return slices.Compact(ids)
}

// Match Arr's RedownloadFailedDownloadService: the main setting gates every
// release, with an additional opt-out for releases grabbed by interactive search.
// Missing/invalid releaseSource is Unknown in Arr, not InteractiveSearch.
func (c *Client) AutomaticHistorySearch(ctx context.Context, releaseSource string) (bool, error) {
	var cfg struct {
		Auto        *bool `json:"autoRedownloadFailed"`
		Interactive *bool `json:"autoRedownloadFailedFromInteractiveSearch"`
	}
	if err := c.do(ctx, http.MethodGet, c.apiPath("config", "downloadclient"), nil, nil, &cfg); err != nil {
		return false, err
	}
	if cfg.Auto == nil {
		return false, errors.New("arr automatic failed-redownload setting is missing")
	}
	if !*cfg.Auto {
		return false, nil
	}
	// Enum.TryParse in Arr accepts both the case-sensitive enum name and its
	// numeric value (InteractiveSearch = 4 in Sonarr and Radarr).
	releaseSource = strings.TrimSpace(releaseSource)
	numeric, _ := strconv.Atoi(releaseSource)
	if releaseSource == "InteractiveSearch" || numeric == 4 {
		if cfg.Interactive == nil {
			return false, errors.New("arr interactive failed-redownload setting is missing")
		}
		return *cfg.Interactive, nil
	}
	return true, nil
}

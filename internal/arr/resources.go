package arr

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Resource reads keep HTTP construction inside the Arr package. Callers compare
// the returned identities against their current remediation scope.
func (c *Client) GetMovie(ctx context.Context, id int) (Movie, error) {
	var movie Movie
	err := c.do(ctx, http.MethodGet, c.apiPath("movie", strconv.Itoa(id)), nil, nil, &movie)
	return movie, err
}

func (c *Client) MovieFiles(ctx context.Context, movieID int) ([]MediaFile, error) {
	var files []MediaFile
	err := c.do(ctx, http.MethodGet, c.apiPath("moviefile"), url.Values{"movieId": {strconv.Itoa(movieID)}}, nil, &files)
	return files, err
}

func (c *Client) Series(ctx context.Context) ([]Series, error) {
	var series []Series
	err := c.do(ctx, http.MethodGet, c.apiPath("series"), nil, nil, &series)
	return series, err
}

func (c *Client) EpisodeFiles(ctx context.Context, seriesID int) ([]MediaFile, error) {
	var files []MediaFile
	err := c.do(ctx, http.MethodGet, c.apiPath("episodefile"), url.Values{"seriesId": {strconv.Itoa(seriesID)}}, nil, &files)
	return files, err
}

package guard

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode"

	"github.com/Prushka/arr-guard/internal/arr"
)

// Keep title comparisons exact after punctuation normalization. Movie releases
// often add "the movie" to a title; no edit-distance or partial-title matching is
// used. A unique catalog title/year match is required when Arr returns no ID.
func importTitle(value string, movie bool) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else if r != '\'' && r != '’' {
			b.WriteByte(' ')
		}
	}
	words := strings.Fields(b.String())
	if movie {
		words = slices.DeleteFunc(words, func(word string) bool { return word == "the" || word == "movie" || word == "film" })
	}
	return strings.Join(words, "")
}

func matchesImportTitle(parsed, title string, movie bool) bool {
	parsed = importTitle(parsed, movie)
	return parsed != "" && parsed == importTitle(title, movie)
}

func (s *Service) resolveImportMovie(ctx context.Context, c *arr.Client, parsed arr.ParsedImport, subject int) error {
	if parsed.Movie != nil {
		if parsed.Movie.ID == subject {
			return nil
		}
		return errors.New("manual import filename identifies a different movie")
	}
	var info struct {
		MovieTitles []string `json:"movieTitles"`
		Year        int      `json:"year"`
	}
	if json.Unmarshal(parsed.MovieInfo, &info) != nil || info.Year < 1 || len(info.MovieTitles) == 0 {
		return errors.New("manual import filename does not independently identify the intended movie")
	}
	movies, err := c.Movies(ctx)
	if err != nil {
		return err
	}
	matched := map[int]bool{}
	for _, movie := range movies {
		if movie.Year != info.Year || movie.ID < 1 {
			continue
		}
		titles := []string{movie.Title, movie.OriginalTitle}
		for _, alias := range movie.AlternateTitles {
			titles = append(titles, alias.Title)
		}
		for _, title := range titles {
			for _, name := range info.MovieTitles {
				if matchesImportTitle(name, title, true) {
					matched[movie.ID] = true
				}
			}
		}
	}
	if len(matched) != 1 || !matched[subject] {
		return errors.New("manual import filename title/year is unknown or ambiguous in the movie catalog")
	}
	return nil
}

type importEpisodeInfo struct {
	SeriesTitle string `json:"seriesTitle"`
	TitleInfo   struct {
		TitleWithoutYear string `json:"titleWithoutYear"`
		Year             int    `json:"year"`
	} `json:"seriesTitleInfo"`
	SeasonNumber           int   `json:"seasonNumber"`
	EpisodeNumbers         []int `json:"episodeNumbers"`
	AbsoluteNumbers        []int `json:"absoluteEpisodeNumbers"`
	SpecialAbsoluteNumbers []int `json:"specialAbsoluteEpisodeNumbers"`
	FullSeason             bool  `json:"fullSeason"`
	MultiSeason            bool  `json:"isMultiSeason"`
	SeasonExtra            bool  `json:"isSeasonExtra"`
	SplitEpisode           bool  `json:"isSplitEpisode"`
}

func (s *Service) resolveImportEpisodes(ctx context.Context, c *arr.Client, parsed arr.ParsedImport, subject int) ([]int, error) {
	if parsed.Series != nil {
		if parsed.Series.ID != subject {
			return nil, errors.New("manual import filename identifies a different series")
		}
		return importEpisodeIDs(parsed.Episodes, subject)
	}
	var info importEpisodeInfo
	if json.Unmarshal(parsed.EpisodeInfo, &info) != nil || info.FullSeason || info.MultiSeason || info.SeasonExtra || info.SplitEpisode || len(info.SpecialAbsoluteNumbers) > 0 {
		return nil, errors.New("manual import filename has no unambiguous per-file episode inventory")
	}
	series, err := c.Series(ctx)
	if err != nil {
		return nil, err
	}
	matched := map[int]bool{}
	// A season-scoped alias must not silently reinterpret another season's
	// numbering. That requires explicit scene/absolute mapping, not arithmetic.
	for _, show := range series {
		if show.ID < 1 || (info.TitleInfo.Year > 0 && show.Year != info.TitleInfo.Year) {
			continue
		}
		titles := []arr.AlternateTitle{{Title: show.Title, SeasonNumber: -1}}
		titles = append(titles, show.AlternateTitles...)
		for _, alias := range titles {
			name := info.SeriesTitle
			if info.TitleInfo.TitleWithoutYear != "" {
				name = info.TitleInfo.TitleWithoutYear
			}
			if !matchesImportTitle(name, alias.Title, false) {
				continue
			}
			if alias.SeasonNumber > 0 && len(info.EpisodeNumbers) > 0 && alias.SeasonNumber != info.SeasonNumber {
				continue
			}
			matched[show.ID] = true
		}
	}
	if len(matched) != 1 || !matched[subject] {
		return nil, errors.New("manual import filename title is unknown or ambiguous in the series catalog")
	}
	episodes, err := c.Episodes(ctx, subject)
	if err != nil {
		return nil, err
	}
	absolute := len(info.AbsoluteNumbers) > 0
	numbers := info.EpisodeNumbers
	if absolute {
		if len(numbers) > 0 {
			return nil, errors.New("manual import filename mixes absolute and seasonal numbering")
		}
		numbers = info.AbsoluteNumbers
	}
	var ids []int
	for _, number := range numbers {
		if number < 1 {
			return nil, errors.New("manual import filename has an invalid episode number")
		}
		matches := map[int]bool{}
		for _, episode := range episodes {
			if absolute {
				if equalNumber(episode.AbsoluteEpisodeNumber, number) || equalNumber(episode.SceneAbsoluteEpisodeNumber, number) {
					matches[episode.ID] = true
				}
			} else if (equalNumber(episode.SeasonNumber, info.SeasonNumber) && equalNumber(episode.EpisodeNumber, number)) || (equalNumber(episode.SceneSeasonNumber, info.SeasonNumber) && equalNumber(episode.SceneEpisodeNumber, number)) {
				matches[episode.ID] = true
			}
		}
		if len(matches) != 1 {
			return nil, errors.New("manual import filename episode mapping is ambiguous or unavailable")
		}
		for id := range matches {
			ids = append(ids, id)
		}
	}
	canonical := arr.CanonicalIDs(ids)
	if len(ids) == 0 || len(canonical) != len(ids) {
		return nil, errors.New("manual import filename episode inventory is empty or duplicated")
	}
	return canonical, nil
}

func equalNumber(value *int, expected int) bool { return value != nil && *value == expected }

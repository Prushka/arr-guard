package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/probe"
)

type importFixture struct {
	f                            *safetyFixture
	candidates                   []arr.ImportCandidate
	parsed                       map[string]arr.ParsedImport
	files                        []arr.MediaFile
	selected                     []arr.ManualImportFile
	status                       string
	valid                        bool
	commandPosts, candidateReads int
	noHistory                    bool
	movieCatalog                 []arr.Movie
	seriesCatalog                []arr.Series
	leaveQueue                   bool
}

func newImportFixture(t *testing.T, kind string) *importFixture {
	t.Helper()
	f := newSafetyFixture(t, kind)
	f.deleted = true
	f.service.config.RecoverBlockedQueue = true
	m := &importFixture{f: f, status: "completed", valid: true, parsed: map[string]arr.ParsedImport{}}
	download := t.TempDir()
	entity := "movie"
	if kind == "sonarr" {
		entity = "series"
	}
	reason := "Found matching " + entity + " via grab history, but release was matched to " + entity + " by ID. Manual Import required."
	ids := []int{10}
	if kind == "sonarr" {
		ids = []int{10, 12}
	}
	f.queue = nil
	for i, id := range ids {
		q := pendingQueueItem(7+i, id, "pack", reason)
		q.OutputPath = download
		f.queue = append(f.queue, q)
		f.history = append(f.history, arr.HistoryRecord{ID: 1000 + i, MovieID: 3, SeriesID: 3, EpisodeID: id, DownloadID: "pack", EventType: "grabbed"})
		name := "Fixture.Movie.2025.mkv"
		if kind == "sonarr" {
			name = "Fixture.Series.S01E" + strconv.Itoa(id) + ".mkv"
		}
		source := filepath.Join(download, name)
		if err := os.WriteFile(source, []byte("fixture media"), 0o600); err != nil {
			t.Fatal(err)
		}
		candidate := arr.ImportCandidate{ID: 80 + i, Path: source, FolderName: "Fixture", Size: 13, DownloadID: "pack", Quality: json.RawMessage(`{"quality":{"id":7,"name":"Bluray-1080p"},"revision":{"version":1,"real":0,"isRepack":false}}`), Languages: json.RawMessage(`[{"id":1,"name":"English"}]`), Rejections: []arr.ImportRejection{}}
		if kind == "radarr" {
			candidate.Movie = &arr.Movie{ID: 3, Year: 2025, Path: filepath.Dir(f.file.Path)}
			m.parsed[name] = arr.ParsedImport{Movie: candidate.Movie}
		} else {
			candidate.Series = &arr.Series{ID: 3, Year: 2025, Path: filepath.Dir(f.file.Path)}
			candidate.Episodes = []arr.Episode{{ID: id, SeriesID: 3, AirDate: "2025-01-01"}}
			m.parsed[name] = arr.ParsedImport{Series: candidate.Series, Episodes: candidate.Episodes}
			candidate.ReleaseType = json.RawMessage(`"singleEpisode"`)
		}
		m.candidates = append(m.candidates, candidate)
	}
	for _, key := range retryKeys(kind, f.file, ids) {
		for range f.service.config.MaxAttempts {
			if _, err := f.service.state.Increment(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.service.probeFn = func(_ context.Context, source string) (probe.Validation, error) {
		info, err := os.Stat(source)
		if err != nil {
			return probe.Validation{}, err
		}
		dir, err := os.Stat(filepath.Dir(source))
		if err != nil {
			return probe.Validation{}, err
		}
		return probe.Validation{Valid: m.valid, HasEnglish: m.valid, HasSubtitles: m.valid, Reason: "fixture subtitles", FileInfo: info, DirectoryInfo: dir, SidecarsChecked: true}, nil
	}
	f.handle = func(w http.ResponseWriter, r *http.Request) bool {
		respond := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/queue/") && m.commandPosts > 0:
			if r.URL.Query().Get("removeFromClient") != "true" || r.URL.Query().Get("blocklist") != "false" || r.URL.Query().Get("skipRedownload") != "true" {
				t.Error("unsafe completed-import cleanup flags")
			}
			if op := f.service.state.Pending()[kind+":queue:pack"]; op.Phase != "manual-import-cleanup-requested" {
				t.Error("cleanup preceded durable intent")
			}
			for _, candidate := range m.candidates {
				if err := os.Remove(candidate.Path); err != nil {
					t.Error(err)
				}
			}
			f.queue = []arr.QueueRecord{}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v3/movie" && m.movieCatalog != nil:
			respond(m.movieCatalog)
		case r.URL.Path == "/api/v3/series" && m.seriesCatalog != nil:
			respond(m.seriesCatalog)
		case r.URL.Path == "/api/v3/manualimport":
			if r.Method != http.MethodGet || r.URL.Query().Get("downloadId") != "pack" || r.URL.Query().Get("seriesId") != "" || r.URL.Query().Get("movieId") != "" || r.URL.Query().Get("filterExistingFiles") != "true" {
				t.Error("invalid candidate discovery request")
			}
			m.candidateReads++
			respond(m.candidates)
		case r.URL.Path == "/api/v3/parse":
			if r.Method != http.MethodGet {
				t.Error("parse was not read-only")
			}
			respond(m.parsed[r.URL.Query().Get("title")])
		case r.URL.Path == "/api/v3/command" && r.Method == http.MethodPost:
			var command struct {
				Name       string                 `json:"name"`
				ImportMode string                 `json:"importMode"`
				Files      []arr.ManualImportFile `json:"files"`
			}
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
				t.Error(err)
			}
			if command.Name != "ManualImport" || command.ImportMode != "auto" {
				t.Error("unexpected import command")
			}
			m.commandPosts++
			m.selected = command.Files
			if pending := f.service.state.Pending()[kind+":queue:pack"]; pending.Phase != "manual-import-requested" || pending.Import == nil {
				t.Error("command submitted before durable journal")
			}
			if m.status == "completed" {
				m.finish(t)
			}
			respond(arr.ImportCommand{ID: 99, Name: "ManualImport", Status: m.status})
		case r.URL.Path == "/api/v3/command/99":
			respond(arr.ImportCommand{ID: 99, Name: "ManualImport", Status: m.status})
		case strings.HasPrefix(r.URL.Path, "/api/v3/moviefile/") || strings.HasPrefix(r.URL.Path, "/api/v3/episodefile/"):
			for _, file := range m.files {
				if strings.HasSuffix(r.URL.Path, "/"+strconv.Itoa(file.ID)) {
					respond(file)
					return true
				}
			}
			return false
		default:
			return false
		}
		return true
	}
	return m
}

// Arr-side mutation simulation is confined to disposable fixture directories.
func (m *importFixture) finish(t *testing.T) {
	t.Helper()
	f := m.f
	for i, selected := range m.selected {
		contents, err := os.ReadFile(selected.Path)
		if err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(filepath.Dir(f.file.Path), strconv.Itoa(i)+".mkv")
		if err := os.WriteFile(dest, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		file := arr.MediaFile{ID: 101 + i, MovieID: 3, ParentID: 3, Path: dest, Size: int64(len(contents)), Year: 2025}
		m.files = append(m.files, file)
		ids := selected.EpisodeIDs
		if f.client.Kind() == "radarr" {
			ids = []int{0}
		}
		for _, id := range ids {
			f.replacementEpisodes = slices.DeleteFunc(f.replacementEpisodes, func(e arr.Episode) bool { return e.ID == id })
			f.replacementEpisodes = append(f.replacementEpisodes, arr.Episode{ID: id, SeriesID: 3, EpisodeFileID: file.ID, AirDate: "2025-01-01"})
			if !m.noHistory {
				f.history = append(f.history, arr.HistoryRecord{ID: 2000 + len(f.history), MovieID: 3, SeriesID: 3, EpisodeID: id, DownloadID: "pack", EventType: "downloadFolderImported", Data: map[string]string{"fileId": strconv.Itoa(file.ID), "droppedPath": selected.Path}})
			}
		}
	}
	f.replacementFiles = m.files
	m.status = "completed"
	if !m.leaveQueue {
		f.queue = []arr.QueueRecord{}
	}
}

func TestMatchedIDManualImportAtExhaustion(t *testing.T) {
	for _, kind := range []string{"radarr", "sonarr"} {
		t.Run(kind, func(t *testing.T) {
			m := newImportFixture(t, kind)
			f := m.f
			attempts, _ := json.Marshal(f.service.state.state.Attempts)
			if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
				t.Fatal(err)
			}
			if m.commandPosts != 1 || len(m.selected) != len(m.candidates) || len(f.mutations) != 1 {
				t.Fatalf("wrong mutations: %v", f.mutations)
			}
			if !f.service.state.IsCompleted(kind+":queue:pack") || len(f.service.state.Pending()) != 0 {
				t.Fatal("actual imported files not reconciled")
			}
			after, _ := json.Marshal(f.service.state.state.Attempts)
			if !bytes.Equal(attempts, after) {
				t.Fatal("fallback consumed or reset retries")
			}
			for _, selected := range m.selected {
				if !bytes.Equal(selected.Quality, m.candidates[0].Quality) || selected.DownloadID != "pack" {
					t.Fatal("import metadata was invented")
				}
			}
			if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
				t.Fatal(err)
			}
			if m.commandPosts != 1 {
				t.Fatal("completed import replayed")
			}
		})
	}
}

func TestMatchedIDManualImportPreflightRefusals(t *testing.T) {
	for _, kind := range []string{"radarr", "sonarr"} {
		for _, scenario := range []string{"dryRun", "noSubtitles", "probeError", "unparsed", "wrongSubject", "extraQueueReason", "candidateRejection", "missingRejections", "wrongDownload", "outsideDownload", "sizeMismatch", "unknownQuality", "duplicate", "staleSource", "staleSidecar", "candidateChanged", "targetAppeared", "queueChanged", "stateFailure"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				m := newImportFixture(t, kind)
				f := m.f
				before, _ := os.ReadFile(f.service.state.path)
				source := m.candidates[0].Path
				switch scenario {
				case "dryRun":
					f.service.config.DryRun = true
					f.client.EnforceReadOnly()
				case "noSubtitles":
					m.valid = false
				case "probeError":
					f.service.probeFn = func(context.Context, string) (probe.Validation, error) {
						return probe.Validation{}, errors.New("input/output error")
					}
				case "unparsed":
					m.parsed = map[string]arr.ParsedImport{}
				case "wrongSubject":
					m.parsed[filepath.Base(source)] = arr.ParsedImport{Movie: &arr.Movie{ID: 4}, Series: &arr.Series{ID: 4}}
				case "extraQueueReason":
					f.queue[0].StatusMessages[0].Messages = append(f.queue[0].StatusMessages[0].Messages, "Caution: Found executable file")
				case "candidateRejection":
					m.candidates[0].Rejections = []arr.ImportRejection{{Reason: "Unable to parse file"}}
				case "missingRejections":
					m.candidates[0].Rejections = nil
				case "wrongDownload":
					m.candidates[0].DownloadID = "different"
				case "outsideDownload":
					m.candidates[0].Path = f.file.Path
				case "sizeMismatch":
					m.candidates[0].Size++
				case "unknownQuality":
					m.candidates[0].Quality = json.RawMessage(`{"quality":{"id":0}}`)
				case "duplicate":
					m.candidates = append(m.candidates, m.candidates[0])
				case "stateFailure":
					f.service.state.path = filepath.Join(source, "state.json")
				default:
					f.before = func(r *http.Request) {
						if r.URL.Path != "/api/v3/manualimport" || m.candidateReads != 1 {
							return
						}
						switch scenario {
						case "staleSource":
							if err := os.WriteFile(source, []byte("changed fixture media"), 0o600); err != nil {
								t.Error(err)
							}
						case "staleSidecar":
							if err := os.WriteFile(strings.TrimSuffix(source, ".mkv")+".en.srt", []byte("subtitle"), 0o600); err != nil {
								t.Error(err)
							}
						case "candidateChanged":
							m.candidates[0].Size++
						case "targetAppeared":
							f.deleted = false
						case "queueChanged":
							f.queue[0].TrackedDownloadState = "importing"
						}
					}
				}
				err := f.service.recoverBlockedQueue(t.Context(), f.client)
				if scenario == "dryRun" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("unsafe import accepted")
				}
				if len(f.mutations) != 0 || len(f.service.state.Pending()) != 0 {
					t.Fatalf("preflight mutated: %v", f.mutations)
				}
				if scenario != "stateFailure" {
					after, _ := os.ReadFile(f.service.state.path)
					if !bytes.Equal(before, after) {
						t.Fatal("preflight changed state")
					}
				}
			})
		}
	}
}

func TestManualImportAcknowledgementIsNotCompletion(t *testing.T) {
	for _, scenario := range []string{"queued", "missingHistory", "failedCommand", "submissionError", "ackPersistenceFailure"} {
		t.Run(scenario, func(t *testing.T) {
			m := newImportFixture(t, "radarr")
			f := m.f
			statePath := f.service.state.path
			switch scenario {
			case "queued":
				m.status = "queued"
			case "missingHistory":
				m.noHistory = true
			case "failedCommand":
				m.status = "failed"
			case "submissionError":
				f.fail = "POST /api/v3/command"
			case "ackPersistenceFailure":
				f.before = func(r *http.Request) {
					if r.Method == http.MethodPost {
						f.service.state.path = filepath.Join(m.candidates[0].Path, "state.json")
					}
				}
			}
			if err := f.service.recoverBlockedQueue(t.Context(), f.client); err == nil {
				t.Fatal("unverified import reported success")
			}
			if len(f.service.state.Pending()) != 1 {
				t.Fatal("uncertain operation not retained")
			}
			mutations := len(f.mutations)
			store, err := LoadStateStore(statePath)
			if err != nil {
				t.Fatal(err)
			}
			f.service.state = store
			f.before = nil
			_ = f.service.CleanupPending(t.Context())
			_ = f.service.recoverBlockedQueue(t.Context(), f.client)
			if len(f.mutations) != mutations {
				t.Fatal("restart resubmitted an uncertain import")
			}
			if scenario == "queued" {
				f.mu.Lock()
				m.finish(t)
				f.mu.Unlock()
				if err := f.service.CleanupPending(t.Context()); err != nil {
					t.Fatal(err)
				}
				if len(store.Pending()) != 0 || !store.IsCompleted("radarr:queue:pack") {
					t.Fatal("late completion not reconciled")
				}
			}
		})
	}
}

func TestManualImportSonarrMappingsAndPartialPack(t *testing.T) {
	for _, scenario := range []string{"partial", "conflictingParse", "overlap", "missingFile", "mixedBudget", "spanningExisting"} {
		t.Run(scenario, func(t *testing.T) {
			m := newImportFixture(t, "sonarr")
			f := m.f
			switch scenario {
			case "partial", "spanningExisting":
				f.replacementEpisodes = []arr.Episode{{ID: 10, SeriesID: 3, EpisodeFileID: 17}, {ID: 12, SeriesID: 3}}
				if scenario == "spanningExisting" {
					m.candidates[1].Episodes = append(m.candidates[1].Episodes, m.candidates[0].Episodes...)
					m.parsed[filepath.Base(m.candidates[1].Path)] = arr.ParsedImport{Series: m.candidates[1].Series, Episodes: m.candidates[1].Episodes}
				}
			case "conflictingParse":
				m.parsed[filepath.Base(m.candidates[0].Path)] = arr.ParsedImport{Series: m.candidates[0].Series, Episodes: m.candidates[1].Episodes}
			case "overlap":
				m.candidates[1].Episodes = m.candidates[0].Episodes
				m.parsed[filepath.Base(m.candidates[1].Path)] = m.parsed[filepath.Base(m.candidates[0].Path)]
			case "missingFile":
				m.candidates = m.candidates[:1]
			case "mixedBudget":
				if err := f.service.state.Reset("sonarr:episodes:12"); err != nil {
					t.Fatal(err)
				}
			}
			err := f.service.recoverBlockedQueue(t.Context(), f.client)
			if scenario == "partial" {
				if err != nil {
					t.Fatal(err)
				}
				if len(m.selected) != 1 || !slices.Equal(m.selected[0].EpisodeIDs, []int{12}) {
					t.Fatal("partial import replaced an existing episode")
				}
			} else if err == nil || len(f.mutations) != 0 {
				t.Fatalf("bad mapping accepted: %v %v", err, f.mutations)
			}
		})
	}
}

func TestConcurrentManualImportSubmitsOnce(t *testing.T) {
	m := newImportFixture(t, "radarr")
	item := m.f.queue[0]
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if err := m.f.service.recoverBlockedQueueItem(t.Context(), m.f.client, item); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if m.commandPosts != 1 {
		t.Fatal("duplicate import commands")
	}
}

func TestManualImportCatalogIdentityFallback(t *testing.T) {
	for _, scenario := range []string{"movie", "movieYearConflict", "movieAmbiguous", "seriesAlias", "absolute", "absoluteAmbiguous", "seasonAliasConflict", "seriesAmbiguous"} {
		t.Run(scenario, func(t *testing.T) {
			kind := "sonarr"
			if strings.HasPrefix(scenario, "movie") {
				kind = "radarr"
			}
			m := newImportFixture(t, kind)
			f := m.f
			if kind == "radarr" {
				m.movieCatalog = []arr.Movie{{ID: 3, Year: 2025, Title: "Doraemon: Nobita's Chronicle of the Moon Exploration"}}
				m.parsed[filepath.Base(m.candidates[0].Path)] = arr.ParsedImport{MovieInfo: json.RawMessage(`{"movieTitles":["Doraemon The Movie Nobitas Chronicle Of The Moon Exploration"],"year":2025}`)}
				if scenario == "movieYearConflict" {
					m.movieCatalog[0].Year = 2024
				}
				if scenario == "movieAmbiguous" {
					conflict := m.movieCatalog[0]
					conflict.ID = 4
					m.movieCatalog = append(m.movieCatalog, conflict)
				}
			} else {
				m.seriesCatalog = []arr.Series{{ID: 3, Title: "Fixture Series", Year: 2025, AlternateTitles: []arr.AlternateTitle{{Title: "Localized Title", SeasonNumber: -1}}}}
				for i, candidate := range m.candidates {
					episode := candidate.Episodes[0].ID
					season := 1
					f.episodes[i].SeasonNumber, f.episodes[i].EpisodeNumber = &season, &episode
					info := importEpisodeInfo{SeriesTitle: "Localized.Title", SeasonNumber: 1, EpisodeNumbers: []int{episode}}
					if strings.HasPrefix(scenario, "absolute") {
						info.EpisodeNumbers, info.AbsoluteNumbers = nil, []int{episode}
						f.episodes[i].AbsoluteEpisodeNumber = &episode
					}
					raw, _ := json.Marshal(info)
					m.parsed[filepath.Base(candidate.Path)] = arr.ParsedImport{EpisodeInfo: raw}
				}
				if scenario == "absoluteAmbiguous" {
					duplicate := 10
					f.episodes[1].SceneAbsoluteEpisodeNumber = &duplicate
				}
				if scenario == "seasonAliasConflict" {
					m.seriesCatalog[0].AlternateTitles[0].SeasonNumber = 2
				}
				if scenario == "seriesAmbiguous" {
					conflict := m.seriesCatalog[0]
					conflict.ID = 4
					m.seriesCatalog = append(m.seriesCatalog, conflict)
				}
			}
			err := f.service.recoverBlockedQueue(t.Context(), f.client)
			allowed := scenario == "movie" || scenario == "seriesAlias" || scenario == "absolute"
			if allowed {
				if err != nil || m.commandPosts != 1 {
					t.Fatalf("valid identity refused: %v", err)
				}
			} else if err == nil || len(f.mutations) > 0 {
				t.Fatalf("conflicting identity imported: %v", err)
			}
		})
	}
}

func TestManualImportProtectsOtherConfiguredArr(t *testing.T) {
	for _, scenario := range []string{"downloadID", "outputPath", "unavailable", "unrelated"} {
		t.Run(scenario, func(t *testing.T) {
			m := newImportFixture(t, "radarr")
			other := newSafetyFixture(t, "sonarr")
			q := pendingQueueItem(90, 10, "other-download", "Unable to parse file")
			q.Status = "downloading"
			switch scenario {
			case "downloadID":
				q.DownloadID = "PACK"
			case "outputPath":
				q.OutputPath = m.f.queue[0].OutputPath
			case "unavailable":
				other.fail = "GET /api/v3/queue"
			}
			other.queue = []arr.QueueRecord{q}
			m.f.service.arr["sonarr"] = other.client
			err := m.f.service.recoverBlockedQueue(t.Context(), m.f.client)
			if scenario == "unrelated" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || len(m.f.mutations) > 0 {
				t.Fatal("shared download was imported")
			}
			if len(other.mutations) > 0 {
				t.Fatal("other server mutated")
			}
		})
	}
}

func TestManualImportLostAcknowledgementCanVerifyWithoutReplay(t *testing.T) {
	m := newImportFixture(t, "radarr")
	f := m.f
	statePath := f.service.state.path
	f.before = func(r *http.Request) {
		if r.Method == http.MethodPost {
			f.service.state.path = filepath.Join(m.candidates[0].Path, "state.json")
		}
	}
	if err := f.service.recoverBlockedQueue(t.Context(), f.client); err == nil {
		t.Fatal("expected acknowledgement persistence failure")
	}
	store, err := LoadStateStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	f.service.state = store
	f.before = nil
	if err := f.service.CleanupPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.commandPosts != 1 || !store.IsCompleted("radarr:queue:pack") {
		t.Fatal("lost acknowledgement could not be reconciled without replay")
	}
}

func TestManualImportRequiresVerifiedLibrarySubtitles(t *testing.T) {
	m := newImportFixture(t, "radarr")
	f := m.f
	m.status = "queued"
	if err := f.service.recoverBlockedQueue(t.Context(), f.client); err == nil {
		t.Fatal("queued command reported complete")
	}
	f.mu.Lock()
	m.finish(t)
	f.mu.Unlock()
	m.valid = false
	if err := f.service.CleanupPending(t.Context()); err == nil {
		t.Fatal("invalid imported media was accepted")
	}
	if len(f.service.state.Pending()) != 1 || m.commandPosts != 1 || len(f.mutations) != 1 {
		t.Fatal("verification failure lost journal or triggered another mutation")
	}
	m.valid = true
	if err := f.service.CleanupPending(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestManualImportStateValidation(t *testing.T) {
	for _, scenario := range []string{"missingDetails", "missingFiles", "negativeCommand", "duplicateEpisodes"} {
		t.Run(scenario, func(t *testing.T) {
			m := newImportFixture(t, "sonarr")
			m.status = "queued"
			_ = m.f.service.recoverBlockedQueue(t.Context(), m.f.client)
			state := m.f.service.state.state
			op := state.Operations["sonarr:queue:pack"]
			switch scenario {
			case "missingDetails":
				op.Import = nil
			case "missingFiles":
				op.Import.Files = nil
			case "negativeCommand":
				op.Import.CommandID = -1
			case "duplicateEpisodes":
				op.Import.Files[1].EpisodeIDs = op.Import.Files[0].EpisodeIDs
			}
			state.Operations["sonarr:queue:pack"] = op
			data, _ := json.Marshal(state)
			statePath := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(statePath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadStateStore(statePath); err == nil {
				t.Fatal("invalid import journal accepted")
			}
		})
	}
}

func TestManualImportCleansResidualPartialPackAfterVerification(t *testing.T) {
	for _, scenario := range []string{"success", "cleanupFailure", "missingExisting", "newConsumer"} {
		t.Run(scenario, func(t *testing.T) {
			m := newImportFixture(t, "sonarr")
			f := m.f
			m.leaveQueue = true
			f.replacementEpisodes = []arr.Episode{{ID: 10, SeriesID: 3, EpisodeFileID: 17}, {ID: 12, SeriesID: 3}}
			if scenario == "cleanupFailure" {
				f.fail = "DELETE /api/v3/queue/7"
			}
			if scenario == "missingExisting" {
				f.before = func(r *http.Request) {
					if r.Method == http.MethodPost {
						f.replacementEpisodes[0].EpisodeFileID = 0
					}
				}
			}
			if scenario == "newConsumer" {
				other := newSafetyFixture(t, "radarr")
				f.service.arr["radarr"] = other.client
				f.before = func(r *http.Request) {
					if r.Method == http.MethodPost {
						other.mu.Lock()
						other.queue = []arr.QueueRecord{pendingQueueItem(88, 0, "pack", "Unable to parse file")}
						other.mu.Unlock()
					}
				}
			}
			err := f.service.recoverBlockedQueue(t.Context(), f.client)
			if scenario == "success" {
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(f.mutations, []string{"POST /api/v3/command", "DELETE /api/v3/queue/7"}) || !f.service.state.IsCompleted("sonarr:queue:pack") {
					t.Fatalf("wrong partial import cleanup: %v", f.mutations)
				}
				for _, candidate := range m.candidates {
					if _, err := os.Stat(candidate.Path); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("download files were preserved by cleanup")
					}
				}
				if data, err := os.ReadFile(f.file.Path); err != nil || string(data) != "fixture media" {
					t.Fatal("existing library file was changed")
				}
			} else {
				if err == nil || len(f.service.state.Pending()) != 1 {
					t.Fatal("unsafe cleanup reported complete")
				}
				if scenario == "cleanupFailure" {
					before := len(f.mutations)
					f.fail = ""
					_ = f.service.CleanupPending(t.Context())
					_ = f.service.recoverBlockedQueue(t.Context(), f.client)
					if len(f.mutations) != before {
						t.Fatal("uncertain cleanup was replayed")
					}
				} else if len(f.mutations) != 1 {
					t.Fatal("unsafe queue cleanup sent")
				}
			}
		})
	}
}

func TestManualImportScanAndServeEntrypoints(t *testing.T) {
	for _, kind := range []string{"radarr", "sonarr"} {
		for _, mode := range []string{"scan", "serve", "disabled"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				m := newImportFixture(t, kind)
				m.f.service.config.RecoverBlockedQueue = mode != "disabled"
				if mode == "scan" {
					if err := m.f.service.Audit(t.Context()); err != nil {
						t.Fatal(err)
					}
				} else {
					m.f.service.runBlockedQueueScan(t.Context())
				}
				want := 1
				if mode == "disabled" {
					want = 0
				}
				if m.commandPosts != want {
					t.Fatalf("import submissions=%d want=%d", m.commandPosts, want)
				}
			})
		}
	}
}

func TestManualImportReadOnlyReconciliationPreservesState(t *testing.T) {
	for _, mode := range []string{"dryRun", "unmatched", "readOnlyClient", "cleanupDisabled"} {
		t.Run(mode, func(t *testing.T) {
			m := newImportFixture(t, "radarr")
			m.status = "queued"
			if err := m.f.service.recoverBlockedQueue(t.Context(), m.f.client); err == nil {
				t.Fatal("queued import reported complete")
			}
			m.leaveQueue = true
			m.f.mu.Lock()
			m.finish(t)
			m.f.mu.Unlock()
			before, _ := os.ReadFile(m.f.service.state.path)
			switch mode {
			case "dryRun":
				m.f.service.config.DryRun = true
			case "unmatched":
				m.f.service.config.Mode = "unmatched"
				m.f.client.EnforceReadOnly()
			case "readOnlyClient":
				m.f.client.EnforceReadOnly()
			case "cleanupDisabled":
				m.f.service.config.RecoverBlockedQueue = false
			}
			_ = m.f.service.CleanupPending(t.Context())
			_ = m.f.service.reconcileManualImports(t.Context(), m.f.client)
			after, _ := os.ReadFile(m.f.service.state.path)
			if !bytes.Equal(before, after) || len(m.f.mutations) != 1 {
				t.Fatal("read-only or disabled cleanup changed state or Arr")
			}
		})
	}
}

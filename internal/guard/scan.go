package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/config"
	"github.com/Prushka/arr-guard/internal/pathutil"
)

func (s *Service) Audit(ctx context.Context) error {
	var auditErrors []error
	if s.config.Workers < 1 {
		return errors.New("audit requires at least one worker")
	}
	for _, client := range s.arr {
		var files []arr.MediaFile
		err := retryScanReads(ctx, func() error {
			var readErr error
			files, readErr = client.ListSubtitleGuardFiles(ctx)
			return readErr
		})
		if err != nil {
			auditErrors = append(auditErrors, fmt.Errorf("audit %s: %w", client.Kind(), err))
			continue
		}
		s.log.Info("library scan started", "arr", client.Kind(), "files", len(files))
		sem := make(chan struct{}, s.config.Workers)
		var wg sync.WaitGroup
		var firstErr error
		var errMu sync.Mutex
		for _, mediaFile := range files {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Wait()
				return ctx.Err()
			}
			file := mediaFile
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if err := s.auditFileWithRetries(ctx, client, file); err != nil {
					s.log.Warn("library file left untouched or requires reconciliation", "arr", client.Kind(), "file_id", file.ID, "error", err)
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}()
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return err
		}
		if firstErr != nil {
			auditErrors = append(auditErrors, fmt.Errorf("audit %s: %w", client.Kind(), firstErr))
		}
		s.log.Info("library scan complete", "arr", client.Kind())
		if s.config.RecoverBlockedQueue {
			if err := s.recoverBlockedQueue(ctx, client); err != nil {
				auditErrors = append(auditErrors, fmt.Errorf("recover %s queue: %w", client.Kind(), err))
			}
		}
	}
	return errors.Join(auditErrors...)
}

// ScanUnmatched lists media files beneath the configured mapped library roots
// that do not have a matching Sonarr or Radarr media-file ID. It intentionally
// does not probe or call applyValidation: an orphan has no Arr media-file ID,
// so subtitle remediation is neither possible nor safe.
func (s *Service) ScanUnmatched(ctx context.Context) error {
	resolvedMappings := make([]config.PathMapping, 0, len(s.config.PathMappings))
	for _, mapping := range s.config.PathMappings {
		root, err := filepath.EvalSymlinks(mapping.To)
		if err != nil {
			return fmt.Errorf("resolve unmatched scan root: %w", err)
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return errors.New("unmatched scan root is not an accessible directory")
		}
		resolvedMappings = append(resolvedMappings, config.PathMapping{To: root})
	}
	roots := scanRoots(resolvedMappings)
	if len(roots) == 0 {
		return errors.New("unmatched scan requires at least one PATH_MAPPINGS_JSON destination path")
	}

	matched := make(map[string]struct{})
	for _, client := range s.arr {
		files, err := client.ListLibraryFiles(ctx)
		if err != nil {
			return fmt.Errorf("list %s library files: %w", client.Kind(), err)
		}
		for _, file := range files {
			if file.ID > 0 && file.SubjectID(client.Kind()) > 0 {
				path := s.mapPath(file.Path)
				if path != "" && path != "." {
					matched[pathutil.Key(pathutil.ResolveExisting(path))] = struct{}{}
				}
			}
		}
	}

	report := UnmatchedReport{
		GeneratedAt: time.Now().UTC(),
		Roots:       roots,
		Files:       make([]UnmatchedMedia, 0),
	}
	scanned := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				if s.isUnmatchedExcludedDir(root, path) {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			if !pathutil.IsMedia(path) {
				return nil
			}
			scanned++
			if _, ok := matched[pathutil.Key(path)]; ok {
				return nil
			}
			report.Files = append(report.Files, UnmatchedMedia{Path: path})
			return nil
		})
		if err != nil {
			return fmt.Errorf("scan unmatched root %s: %w", root, err)
		}
	}
	sort.Slice(report.Files, func(i, j int) bool { return pathutil.Key(report.Files[i].Path) < pathutil.Key(report.Files[j].Path) })
	if err := writeUnmatchedReport(s.config.UnmatchedPath, report); err != nil {
		return err
	}
	s.log.Info("unmatched scan complete", "roots", len(roots), "media_files", scanned, "unmatched_files", len(report.Files), "output", s.config.UnmatchedPath)
	return nil
}

func (s *Service) isUnmatchedExcludedDir(root, path string) bool {
	pathKey := pathutil.Key(path)
	for _, excluded := range s.config.UnmatchedExcludeDirs {
		excluded = strings.TrimSpace(excluded)
		if excluded == "" {
			continue
		}
		excludedKey := pathutil.Key(pathutil.ResolveExisting(excluded))
		if !filepath.IsAbs(filepath.Clean(excluded)) {
			excludedKey = pathutil.Key(filepath.Join(root, excluded))
		}
		if pathKey == excludedKey {
			return true
		}
	}
	return false
}

func scanRoots(mappings []config.PathMapping) []string {
	values := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		if root := strings.TrimSpace(mapping.To); root != "" {
			values = append(values, filepath.Clean(root))
		}
	}
	sort.Slice(values, func(i, j int) bool {
		return len(values[i]) < len(values[j])
	})
	roots := make([]string, 0, len(values))
	for _, candidate := range values {
		duplicate := false
		for _, root := range roots {
			relative, err := filepath.Rel(root, candidate)
			if err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			roots = append(roots, candidate)
		}
	}
	return roots
}

func writeUnmatchedReport(path string, report UnmatchedReport) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("UNMATCHED_PATH must not be empty")
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode unmatched report: %w", err)
	}
	data = append(data, '\n')
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create unmatched report directory: %w", err)
		}
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write unmatched report: %w", err)
	}
	return nil
}

package pathutil

import (
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
)

func Within(root, target string) bool {
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, target)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

// Resolve the existing ancestor too, so a not-yet-created JSON file cannot hide
// beneath a media mount reached through a symlink or Windows junction.
func ResolveExisting(value string) string {
	value, err := filepath.Abs(value)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(value); err == nil {
		return resolved
	}
	parent := filepath.Dir(value)
	if parent == value {
		return value
	}
	return filepath.Join(ResolveExisting(parent), filepath.Base(value))
}

func ComparableArr(value, from string) (string, string) {
	value, from = Normalize(value), Normalize(from)
	if strings.HasPrefix(from, "//") || (len(from) > 1 && from[1] == ':') {
		value, from = strings.ToLower(value), strings.ToLower(from)
	}
	return value, from
}

func MappedSuffix(value, from string) string {
	comparableValue, comparableFrom := ComparableArr(value, from)
	if comparableValue == comparableFrom {
		return ""
	}
	// The number of path separators stays stable even when Unicode case folding
	// changes the byte length of a Windows source prefix.
	separators := strings.Count(strings.TrimRight(from, "/"), "/") + 1
	for i, c := range value {
		if c == '/' {
			separators--
			if separators == 0 {
				return value[i:]
			}
		}
	}
	return ""
}

func SameFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func Key(value string) string {
	value = filepath.Clean(strings.TrimSpace(value))
	if runtime.GOOS == "windows" {
		return strings.ToLower(value)
	}
	return value
}

func Normalize(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if value == "" {
		return ""
	}
	clean := pathpkg.Clean(value)
	if strings.HasPrefix(value, "//") {
		clean = "/" + clean
	}
	return clean
}

func Mapped(to, suffix, originalTo string) string {
	result := pathpkg.Join(to, suffix)
	if strings.HasPrefix(to, "//") {
		result = "/" + result
	}
	if strings.Contains(originalTo, "\\") {
		result = strings.ReplaceAll(result, "/", "\\")
	}
	return result
}

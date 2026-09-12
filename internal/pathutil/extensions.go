package pathutil

import (
	"path/filepath"
	"strings"
)

var subtitleExtensions = map[string]struct{}{
	".ass":  {},
	".dfxp": {},
	".idx":  {},
	".mks":  {},
	".mpl2": {},
	".pgs":  {},
	".sami": {},
	".smi":  {},
	".scc":  {},
	".ssa":  {},
	".srt":  {},
	".sub":  {},
	".sup":  {},
	".stl":  {},
	".ttml": {},
	".usf":  {},
	".vtt":  {},
}

func IsSubtitle(path string) bool {
	_, ok := subtitleExtensions[strings.ToLower(filepath.Ext(path))]
	return ok
}

var mediaExtensions = map[string]struct{}{
	".264": {}, ".265": {}, ".3g2": {}, ".3gp": {}, ".asf": {}, ".avi": {}, ".divx": {},
	".dvr-ms": {}, ".f4v": {}, ".flv": {}, ".iso": {}, ".m2ts": {},
	".h264": {}, ".h265": {}, ".hevc": {}, ".m3u": {}, ".m3u8": {},
	".m2v": {}, ".m4v": {}, ".mkv": {}, ".mov": {}, ".mp4": {}, ".mpe": {},
	".mpeg": {}, ".mpg": {}, ".mpv2": {}, ".mts": {}, ".mxf": {}, ".ogm": {},
	".ogv": {}, ".rm": {}, ".rmvb": {}, ".ts": {}, ".vob": {},
	".webm": {}, ".wmv": {}, ".wtv": {}, ".xvid": {},
}

func IsMedia(value string) bool {
	_, ok := mediaExtensions[strings.ToLower(filepath.Ext(value))]
	return ok
}

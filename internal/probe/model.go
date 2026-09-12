package probe

import (
	"os"
)

type ProbeResult struct {
	Streams []ProbeStream `json:"streams"`
	Format  struct {
		Name string `json:"format_name"`
	} `json:"format"`
}

type ProbeStream struct {
	CodecType   string            `json:"codec_type"`
	CodecName   string            `json:"codec_name"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Tags        map[string]string `json:"tags"`
	Disposition map[string]int    `json:"disposition"`
}

type Validation struct {
	FileInfo           os.FileInfo        `json:"-"`
	DirectoryInfo      os.FileInfo        `json:"-"`
	Sidecars           []ExternalSubtitle `json:"-"`
	SidecarsChecked    bool               `json:"-"`
	Valid              bool               `json:"valid"`
	HasSubtitles       bool               `json:"hasSubtitles"`
	HasEnglish         bool               `json:"hasEnglishSubtitles"`
	HasUnknownLanguage bool               `json:"hasUnknownSubtitleLanguage"`
	SubtitleLangs      []string           `json:"subtitleLanguages,omitempty"`
	ProbeWarnings      []string           `json:"probeWarnings,omitempty"`
	Reason             string             `json:"reason,omitempty"`
}

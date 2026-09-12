package guard

import (
	"time"
)

type UnmatchedReport struct {
	GeneratedAt time.Time        `json:"generatedAt"`
	Roots       []string         `json:"roots"`
	Files       []UnmatchedMedia `json:"files"`
}

type UnmatchedMedia struct {
	Path string `json:"path"`
}

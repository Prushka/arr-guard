package arr

func (p WebhookPayload) Files(kind string) []WebhookFile {
	if kind == "sonarr" {
		if len(p.EpisodeFiles) > 0 {
			return p.EpisodeFiles
		}
		if p.EpisodeFile != nil {
			return []WebhookFile{*p.EpisodeFile}
		}
		return nil
	}
	if len(p.MovieFiles) > 0 {
		return p.MovieFiles
	}
	if p.MovieFile != nil {
		return []WebhookFile{*p.MovieFile}
	}
	return nil
}

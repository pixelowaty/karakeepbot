package karakeepbot

import (
	"strings"
	"unicode"

	"github.com/Madh93/go-karakeep"
)

// KarakeepBookmark represents a bookmark received from the karakeep API.
type KarakeepBookmark karakeep.Bookmark

// Attrs returns a slice of logging attributes for the bookmark.
func (kb KarakeepBookmark) Attrs() []any {
	return []any{
		"scope", "karakeep",
		"bookmark_id", kb.Id,
		"tagging_status", *kb.TaggingStatus,
	}
}

// AttrsWithError returns a slice of logging attributes for the bookmark with an
// error.
func (kb KarakeepBookmark) AttrsWithError(err error) []any {
	return append(kb.Attrs(), "error", err)
}

// HasTitle reports whether the bookmark has a non-empty title.
func (kb KarakeepBookmark) HasTitle() bool {
	return kb.Title != nil && strings.TrimSpace(*kb.Title) != ""
}

// GenerateTitleFromAI derives a title for the bookmark from AI-generated data.
// It uses the first sentence of the AI summary, truncated to at most maxRunes
// UTF-8 runes. If the summary is empty, it falls back to the comma-separated
// names of tags attached by the AI (AttachedBy == "ai").
func (kb KarakeepBookmark) GenerateTitleFromAI(maxRunes int) string {
	if kb.Summary != nil {
		if title := firstSentence(*kb.Summary, maxRunes); title != "" {
			return title
		}
	}

	var aiTags []string
	for _, tag := range kb.Tags {
		if tag.AttachedBy == karakeep.Ai {
			aiTags = append(aiTags, tag.Name)
		}
	}
	return strings.Join(aiTags, ", ")
}

// firstSentence extracts the first sentence of text, truncated to at most
// maxRunes UTF-8 runes. A sentence ends at the first '.', '!', or '?' that is
// followed by whitespace or end-of-string.
func firstSentence(text string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(text))

	// Find the end of the first sentence.
	end := len(runes)
	for i, r := range runes {
		if r == '.' || r == '!' || r == '?' {
			// Only treat as sentence end if followed by whitespace or end.
			if i+1 >= len(runes) || unicode.IsSpace(runes[i+1]) {
				end = i + 1
				break
			}
		}
	}
	sentence := string(runes[:end])

	// Truncate safely by runes if it exceeds maxRunes. The ellipsis is
	// included in the maxRunes budget so the result is at most maxRunes.
	if end > maxRunes {
		if maxRunes <= 3 {
			return strings.TrimSpace(string(runes[:maxRunes]))
		}
		sentence = string(runes[:maxRunes-3]) + "..."
	}

	return strings.TrimSpace(sentence)
}

// Hashtags returns a string of hashtags associated with the bookmark.
func (kb KarakeepBookmark) Hashtags() string {
	var tags []string
	for _, tag := range kb.Tags {
		name := sanitizeTag(tag.Name)
		tags = append(tags, "#"+name)
	}
	return strings.Join(tags, " ")
}

// sanitizeTag removes any spaces or hyphens from the tag name.
func sanitizeTag(tag string) string {
	tag = strings.ReplaceAll(tag, " ", "")
	return strings.ReplaceAll(tag, "-", "")
}

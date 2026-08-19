package community

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Hashtag extraction (docs/COMMUNITY-FEED.md).
//
// Tags are DERIVED from post content at write time - the author's text is the
// only source of truth, and community_post_hashtags is replaced whenever the
// content changes, so the two can never disagree. Extraction lives here, in
// Go, because Thai has no word boundaries a database regex could lean on and
// because the rule must be testable beside the rest of the domain.

// MaxHashtagRunes bounds one tag; the column is VARCHAR(64) and a "tag"
// longer than this is prose wearing a # rather than a label.
const MaxHashtagRunes = 64

// MaxHashtagsPerPost bounds how many tags one post may contribute, so a
// tag-stuffed post cannot own the trending panel by itself.
const MaxHashtagsPerPost = 10

// hashtagPattern matches # followed by letters (Thai included), combining
// marks (Thai vowels and tone marks are \p{M}), digits, or underscores.
// Anything else - whitespace, punctuation, a second # - ends the tag.
var hashtagPattern = regexp.MustCompile(`#([\p{L}\p{M}\p{N}_]+)`)

// ExtractHashtags returns the normalized, deduplicated hashtags of a post's
// content, in order of first appearance, capped at MaxHashtagsPerPost.
//
// Normalization is lowercasing only - it folds #OOC and #ooc together without
// touching Thai, which has no case. A tag longer than MaxHashtagRunes is
// dropped rather than truncated: a truncated tag would be a tag the author
// never wrote.
func ExtractHashtags(content string) []string {
	matches := hashtagPattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(matches))
	tags := make([]string, 0, len(matches))
	for _, match := range matches {
		tag := strings.ToLower(match[1])
		if utf8.RuneCountInString(tag) > MaxHashtagRunes {
			continue
		}
		if _, dup := seen[tag]; dup {
			continue
		}
		seen[tag] = struct{}{}
		tags = append(tags, tag)
		if len(tags) == MaxHashtagsPerPost {
			break
		}
	}
	return tags
}

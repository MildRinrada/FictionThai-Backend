package profiles

// The profile WRITE surface (docs/PROFILE-AND-ACHIEVEMENTS.md Part 1).
//
// Repository fact worth stating once: user_profiles was readable everywhere and
// writable nowhere - the only mutation in the whole backend was SetAvatarURL
// from a media upload. A person could not change their own display name, their
// own introduction, or their own links. This file is that missing path.
//
// It lives beside the public read but shares nothing with it. Get() stays
// identity-free so one cached response still serves every visitor; Update()
// takes an identity and writes exactly one row - the CALLER's. There is no
// cross-user profile edit and no admin override here.

import (
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// PinnedInput is one pin as the owner sends it: which work, and why.
type PinnedInput struct {
	NovelID string `json:"novel_id"`
	Note    string `json:"note"`
}

// Field bounds. display_name is VARCHAR(64) in the schema; the rest are TEXT,
// so these are product limits rather than storage ones.
const (
	displayNameMaxRunes = 64
	bioMaxRunes         = 2000
	linkLabelMaxRunes   = 24
	urlMaxLength        = 2048
	maxLinks            = 6
	maxPinned           = 3
	pinNoteMaxRunes     = 80
	boundariesMaxRunes  = 1000
)

// Link is one writer-published contact. The label is the writer's own word for
// it - "X", "คอมมิชชัน", "เว็บส่วนตัว" - because the platform has no business
// maintaining a list of which services exist.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Edit is a partial profile update: a nil field is UNTOUCHED, an empty string
// CLEARS. That distinction is why every field is a pointer - a PATCH that sent
// zero values for everything the client did not know about would quietly erase
// fields added after that client shipped.
type Edit struct {
	DisplayName *string `json:"display_name"`
	Bio         *string `json:"bio"`
	WebsiteURL  *string `json:"website_url"`
	Links       *[]Link `json:"links"`
	// OpenFor is the writer's current availability - see openForVocabulary.
	// Setting it creates the author_profiles row on demand, the same
	// documented behaviour the donation link has.
	OpenFor *[]string `json:"open_for"`

	// Boundaries is คำเตือน/ขอบเขตของนักเขียน: what this writer will and will
	// not write. Free text, deliberately - the opposite of OpenFor above, which
	// is a closed vocabulary because it is a status read at a glance. This one
	// is a sentence only its author can write, so the platform stores it and
	// shows it and does nothing else to it. Like OpenFor, setting it creates
	// the author_profiles row on demand.
	Boundaries *string `json:"boundaries"`

	// Pinned is the writer's own shelf: up to three of their works, in their
	// order, each with one line of their own words. Sending [] clears it.
	Pinned *[]PinnedInput `json:"pinned"`

	// WallEnabled is the profile wall's on/off switch. A bool pointer like
	// everything else here: absent leaves it alone, which is what lets a client
	// that predates the wall save a profile without closing it.
	WallEnabled *bool `json:"wall_enabled"`

	// HideFromRankings is the writer's opt-out from the home page's writer
	// rankings (docs/WRITER-SPOTLIGHT.md). Same pointer contract as the wall.
	HideFromRankings *bool `json:"hide_from_rankings"`
}

// openForVocabulary is what a writer may declare themselves open to. A closed
// list on purpose: it is a status other people read at a glance, not a free
// text field to advertise in.
var openForVocabulary = map[string]bool{
	"commission": true, // รับคอมมิชชัน
	"request":    true, // เปิดรับฟิคขอ
	"beta":       true, // รับเบต้าอ่าน
}

// clean normalises and validates an Edit, returning the values to write.
func (e *Edit) clean() (*Edit, error) {
	fields := map[string][]string{}
	out := &Edit{}

	if e.DisplayName != nil {
		name := strings.TrimSpace(*e.DisplayName)
		if utf8.RuneCountInString(name) > displayNameMaxRunes {
			fields["display_name"] = []string{"ชื่อที่แสดงยาวเกินไป (ไม่เกิน 64 ตัวอักษร)"}
		}
		out.DisplayName = &name
	}
	if e.Bio != nil {
		bio := strings.TrimSpace(*e.Bio)
		if utf8.RuneCountInString(bio) > bioMaxRunes {
			fields["bio"] = []string{"แนะนำตัวยาวเกินไป (ไม่เกิน 2,000 ตัวอักษร)"}
		}
		out.Bio = &bio
	}
	if e.WebsiteURL != nil {
		site, err := cleanURL(*e.WebsiteURL)
		if err != nil {
			fields["website_url"] = []string{err.Error()}
		}
		out.WebsiteURL = &site
	}
	if e.Links != nil {
		links := make([]Link, 0, len(*e.Links))
		for _, link := range *e.Links {
			label := strings.TrimSpace(link.Label)
			href, err := cleanURL(link.URL)
			if err != nil {
				fields["links"] = []string{err.Error()}
				break
			}
			// A link with no destination is a row the writer left blank, not an
			// error worth refusing the whole save over.
			if href == "" {
				continue
			}
			if label == "" {
				label = hostOf(href)
			}
			if utf8.RuneCountInString(label) > linkLabelMaxRunes {
				fields["links"] = []string{"ชื่อลิงก์ยาวเกินไป (ไม่เกิน 24 ตัวอักษร)"}
				break
			}
			links = append(links, Link{Label: label, URL: href})
		}
		if len(links) > maxLinks {
			fields["links"] = []string{"ใส่ลิงก์ได้ไม่เกิน 6 รายการ"}
		}
		out.Links = &links
	}
	if e.OpenFor != nil {
		seen := map[string]bool{}
		open := make([]string, 0, len(*e.OpenFor))
		for _, kind := range *e.OpenFor {
			kind = strings.TrimSpace(kind)
			if !openForVocabulary[kind] {
				fields["open_for"] = []string{"สถานะการรับงานไม่ถูกต้อง"}
				break
			}
			if seen[kind] {
				continue
			}
			seen[kind] = true
			open = append(open, kind)
		}
		out.OpenFor = &open
	}
	if e.Boundaries != nil {
		// Length is the ONLY check. There is no vocabulary to validate against
		// and there never will be: normalising "ไม่รับเรื่องที่มีตัวละครเด็ก"
		// into a taxonomy would be the platform editing a warning its author
		// wrote for a reason.
		boundaries := strings.TrimSpace(*e.Boundaries)
		if utf8.RuneCountInString(boundaries) > boundariesMaxRunes {
			fields["boundaries"] = []string{"ข้อความยาวเกินไป (ไม่เกิน 1,000 ตัวอักษร)"}
		}
		out.Boundaries = &boundaries
	}
	if e.Pinned != nil {
		// Ownership and readability are NOT checked here - they are re-checked
		// on every read, so a work that later goes private simply stops
		// showing rather than leaking its title from a stale pin.
		pins := make([]PinnedInput, 0, len(*e.Pinned))
		seen := map[string]bool{}
		for _, pin := range *e.Pinned {
			id := strings.TrimSpace(pin.NovelID)
			if id == "" || seen[id] {
				continue
			}
			if _, err := uuid.Parse(id); err != nil {
				fields["pinned"] = []string{"เรื่องที่ปักหมุดไม่ถูกต้อง"}
				break
			}
			note := strings.TrimSpace(pin.Note)
			if utf8.RuneCountInString(note) > pinNoteMaxRunes {
				fields["pinned"] = []string{"ข้อความใต้เรื่องยาวเกินไป (ไม่เกิน 80 ตัวอักษร)"}
				break
			}
			seen[id] = true
			pins = append(pins, PinnedInput{NovelID: id, Note: note})
		}
		if len(pins) > maxPinned {
			fields["pinned"] = []string{"ปักหมุดได้ไม่เกิน 3 เรื่อง"}
		}
		out.Pinned = &pins
	}
	if e.WallEnabled != nil {
		enabled := *e.WallEnabled
		out.WallEnabled = &enabled
	}
	if e.HideFromRankings != nil {
		hidden := *e.HideFromRankings
		out.HideFromRankings = &hidden
	}

	if len(fields) > 0 {
		return nil, apierror.Validation(fields)
	}
	return out, nil
}

// cleanURL accepts an absolute http(s) URL, or "" to clear. Anything else is
// refused: a profile link is a destination readers are sent to, so javascript:,
// data:, and host-less values have no place in it.
func cleanURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > urlMaxLength {
		return "", errMessage("ลิงก์ยาวเกินไป")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", errMessage("ลิงก์ต้องเป็น URL เต็มที่ขึ้นต้นด้วย https://")
	}
	return raw, nil
}

func hostOf(raw string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return strings.TrimPrefix(parsed.Host, "www.")
	}
	return raw
}

type errMessage string

func (e errMessage) Error() string { return string(e) }

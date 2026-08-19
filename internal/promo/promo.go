// Package promo owns the home hero's curated slide queue
// (docs/HOME-PROMO.md).
//
// Slides are CONTENT the staff schedules - each with its own art, copy, and
// window - never a banner network. The two integrity rules the document makes
// binding are enforced here, at read time, not merely promised: paid slides
// are capped at one-in-four, and every paid slide is labelled by `source` so
// the client can render its "โปรโมท" chip.
//
// There is NO payment flow anywhere in this package: `paid` is a label the
// staff sets on a slide arranged off-platform (docs/MONETIZATION.md §24 blocks
// new money streams).
package promo

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Source is who put a slide in the queue.
type Source string

const (
	// SourceEditorial is the staff's own pick.
	SourceEditorial Source = "editorial"
	// SourcePaid is a bought placement - always labelled, always capped.
	SourcePaid Source = "paid"
	// SourceEvent is the platform's own campaign.
	SourceEvent Source = "event"
)

func (s Source) Valid() bool {
	switch s {
	case SourceEditorial, SourcePaid, SourceEvent:
		return true
	}
	return false
}

// TextSide is where the copy sits over the banner art.
type TextSide string

const (
	TextStart TextSide = "start"
	TextEnd   TextSide = "end"
)

func (t TextSide) Valid() bool { return t == TextStart || t == TextEnd }

// MaxServed is how many slides one response carries. The review asked for a
// 3-4 slide deck; more than four auto-advancing slides is a slideshow nobody
// finishes.
const MaxServed = 4

// Slide mirrors one promo_slides row.
type Slide struct {
	ID       uuid.UUID
	Position int

	Kicker   string
	Headline string
	Tagline  string
	CTALabel string

	LinkURL  string
	ImageURL *string
	BgColor  *string
	TextSide TextSide

	Source  Source
	Enabled bool

	StartsAt *time.Time
	EndsAt   *time.Time

	Impressions int64
	Clicks      int64

	CreatedAt time.Time
	UpdatedAt time.Time
}

// LiveAt reports whether the slide's window covers the moment.
func (s Slide) LiveAt(now time.Time) bool {
	if !s.Enabled {
		return false
	}
	if s.StartsAt != nil && now.Before(*s.StartsAt) {
		return false
	}
	if s.EndsAt != nil && !now.Before(*s.EndsAt) {
		return false
	}
	return true
}

// View is a slide as the PUBLIC endpoint serves it - copy, art, link, and the
// source (the client's "โปรโมท" label keys off it). The counters stay
// admin-only: a slide's numbers are the buyer's business, not the visitor's.
type View struct {
	ID       uuid.UUID `json:"id"`
	Kicker   string    `json:"kicker,omitempty"`
	Headline string    `json:"headline"`
	Tagline  string    `json:"tagline,omitempty"`
	CTALabel string    `json:"cta_label,omitempty"`
	LinkURL  string    `json:"link_url"`
	ImageURL *string   `json:"image_url,omitempty"`
	BgColor  *string   `json:"bg_color,omitempty"`
	TextSide TextSide  `json:"text_side"`
	Source   Source    `json:"source"`
}

func (s Slide) View() View {
	return View{
		ID: s.ID, Kicker: s.Kicker, Headline: s.Headline, Tagline: s.Tagline,
		CTALabel: s.CTALabel, LinkURL: s.LinkURL, ImageURL: s.ImageURL,
		BgColor: s.BgColor, TextSide: s.TextSide, Source: s.Source,
	}
}

// AdminView is the queue as the staff page reads it - everything, counters
// included.
type AdminView struct {
	ID       uuid.UUID `json:"id"`
	Position int       `json:"position"`

	Kicker   string   `json:"kicker"`
	Headline string   `json:"headline"`
	Tagline  string   `json:"tagline"`
	CTALabel string   `json:"cta_label"`
	LinkURL  string   `json:"link_url"`
	ImageURL *string  `json:"image_url,omitempty"`
	BgColor  *string  `json:"bg_color,omitempty"`
	TextSide TextSide `json:"text_side"`
	Source   Source   `json:"source"`

	Enabled  bool       `json:"enabled"`
	StartsAt *time.Time `json:"starts_at,omitempty"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`

	Impressions int64 `json:"impressions"`
	Clicks      int64 `json:"clicks"`

	UpdatedAt time.Time `json:"updated_at"`
}

func (s Slide) AdminView() AdminView {
	return AdminView{
		ID: s.ID, Position: s.Position,
		Kicker: s.Kicker, Headline: s.Headline, Tagline: s.Tagline,
		CTALabel: s.CTALabel, LinkURL: s.LinkURL, ImageURL: s.ImageURL,
		BgColor: s.BgColor, TextSide: s.TextSide, Source: s.Source,
		Enabled: s.Enabled, StartsAt: s.StartsAt, EndsAt: s.EndsAt,
		Impressions: s.Impressions, Clicks: s.Clicks, UpdatedAt: s.UpdatedAt,
	}
}

// Input is one slide as the admin form submits it.
type Input struct {
	Kicker   string  `json:"kicker"`
	Headline string  `json:"headline"`
	Tagline  string  `json:"tagline"`
	CTALabel string  `json:"cta_label"`
	LinkURL  string  `json:"link_url"`
	ImageURL *string `json:"image_url"`
	BgColor  *string `json:"bg_color"`
	TextSide string  `json:"text_side"`
	Source   string  `json:"source"`

	Enabled  bool       `json:"enabled"`
	StartsAt *time.Time `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at"`
}

var bgColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Validate normalises and checks one slide's fields.
func Validate(in Input) (Slide, error) {
	fields := map[string][]string{}

	slide := Slide{
		Kicker:   strings.TrimSpace(in.Kicker),
		Headline: strings.TrimSpace(in.Headline),
		Tagline:  strings.TrimSpace(in.Tagline),
		CTALabel: strings.TrimSpace(in.CTALabel),
		LinkURL:  strings.TrimSpace(in.LinkURL),
		Enabled:  in.Enabled,
		StartsAt: in.StartsAt,
		EndsAt:   in.EndsAt,
	}

	if slide.Headline == "" || utf8.RuneCountInString(slide.Headline) > 120 {
		fields["headline"] = []string{"A headline of 1-120 characters is required."}
	}
	if utf8.RuneCountInString(slide.Kicker) > 40 {
		fields["kicker"] = []string{"At most 40 characters."}
	}
	if utf8.RuneCountInString(slide.Tagline) > 160 {
		fields["tagline"] = []string{"At most 160 characters."}
	}
	if utf8.RuneCountInString(slide.CTALabel) > 40 {
		fields["cta_label"] = []string{"At most 40 characters."}
	}

	// An INTERNAL path, always. "/novel/x" is a slide; "https://elsewhere" is
	// an open redirect wearing a hero image, and "//elsewhere" is the same
	// trick without a scheme.
	if slide.LinkURL == "" || !strings.HasPrefix(slide.LinkURL, "/") ||
		strings.HasPrefix(slide.LinkURL, "//") ||
		utf8.RuneCountInString(slide.LinkURL) > 500 {
		fields["link_url"] = []string{"An internal path starting with / is required."}
	}

	if in.ImageURL != nil {
		trimmed := strings.TrimSpace(*in.ImageURL)
		if trimmed != "" {
			if utf8.RuneCountInString(trimmed) > 500 {
				fields["image_url"] = []string{"At most 500 characters."}
			}
			slide.ImageURL = &trimmed
		}
	}
	if in.BgColor != nil {
		trimmed := strings.ToLower(strings.TrimSpace(*in.BgColor))
		if trimmed != "" {
			if !bgColorPattern.MatchString(trimmed) {
				fields["bg_color"] = []string{"A colour in #rrggbb form."}
			}
			slide.BgColor = &trimmed
		}
	}

	slide.TextSide = TextSide(strings.TrimSpace(in.TextSide))
	if slide.TextSide == "" {
		slide.TextSide = TextStart
	}
	if !slide.TextSide.Valid() {
		fields["text_side"] = []string{"One of: start, end."}
	}

	slide.Source = Source(strings.TrimSpace(in.Source))
	if slide.Source == "" {
		slide.Source = SourceEditorial
	}
	if !slide.Source.Valid() {
		fields["source"] = []string{"One of: editorial, paid, event."}
	}

	if slide.StartsAt != nil && slide.EndsAt != nil && !slide.EndsAt.After(*slide.StartsAt) {
		fields["ends_at"] = []string{"The end must come after the start."}
	}

	if len(fields) > 0 {
		return Slide{}, apierror.Validation(fields)
	}
	return slide, nil
}

// ServeQueue applies the deck rules to the LIVE slides, in position order:
// at most MaxServed slides, and at most ONE paid slide - included only when
// at least three editorial/event slides ride beside it, so a bought placement
// is never more than a quarter of the deck and never the deck itself
// (docs/HOME-PROMO.md "integrity rules").
func ServeQueue(live []Slide) []Slide {
	editorial := 0
	for _, slide := range live {
		if slide.Source != SourcePaid {
			editorial++
		}
	}
	allowPaid := editorial >= 3

	served := make([]Slide, 0, MaxServed)
	paidUsed := false
	for _, slide := range live {
		if len(served) == MaxServed {
			break
		}
		if slide.Source == SourcePaid {
			if !allowPaid || paidUsed {
				continue
			}
			paidUsed = true
		}
		served = append(served, slide)
	}
	return served
}

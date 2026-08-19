// Package media implements uploaded-file metadata and lifecycle
// (docs/08 §22 and §44 Phase 9, docs/09 §27, docs/07 §22–§23, docs/11
// §28–§29).
//
// The package owns the media table and the storage lifecycle - and NOTHING
// else. The binary bytes live behind the storage.Store boundary; owning
// domains keep owning their entities: attaching a cover goes through the
// novels service (which enforces ownership), attaching an avatar writes the
// uploader's OWN profile row. Media never re-implements another domain's
// authorization (docs/10 §27).
package media

import (
	"time"

	"github.com/google/uuid"
)

// Type is the media purpose (docs/08 §22.1). Stored as VARCHAR: the list is
// the vocabulary, and widening it must not need a migration.
type Type string

const (
	TypeAvatar     Type = "avatar"
	TypeNovelCover Type = "novel_cover"
	// TypeProfileBanner is the cover image across the top of a profile
	// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 1). Like an avatar it attaches to
	// the uploader's OWN profile row and to nothing else, so the uploader is
	// the only possible owner and no cross-domain authorization is involved.
	TypeProfileBanner Type = "profile_banner"
	// TypeEntryImage is a picture on a headcanon entry (13M). Unlike a cover or
	// an avatar it attaches to NOTHING at upload time: the entry it belongs to
	// may still be an unsaved row in the writer's editor, and the whole topic is
	// written by the chapter PATCH that follows. Authorization still happens
	// here and before any byte is stored - the uploader must be the fiction's
	// writer - which is the part that cannot wait.
	TypeEntryImage Type = "entry_image"
	// TypeChapterImage is a picture inside a chapter's PROSE (13N). Same rule as
	// TypeEntryImage and for the same reason - the paragraph it belongs to is
	// just text the writer has not saved yet - but a distinct purpose, because
	// the storage key and any future moderation queue are namespaced by it.
	TypeChapterImage Type = "chapter_image"
	// TypeCharacterAvatar is a cast member's portrait (Phase 12A). Same
	// attach-later contract as entry/chapter images: the character row may not
	// exist yet when the picture is chosen, so the URL lands in
	// characters.avatar_url via the characters PATCH that follows. The uploader
	// must be able to EDIT the fiction's content - which includes collaborators,
	// not just the owner (13U).
	TypeCharacterAvatar Type = "character_avatar"
	// TypeCommunityImage and TypeAttachment are §22.1 vocabulary with no
	// owning surface yet (community posts are text-only per the Phase 7
	// reconciliation; attachments have no consumer). They exist so the
	// column's meaning is complete; UploadableType refuses them until a
	// surface exists.
	TypeCommunityImage Type = "community_image"
	TypeAttachment     Type = "attachment"
	// TypePromoBanner is the wide art on one home-hero slide
	// (docs/HOME-PROMO.md). Staff-only at upload, and attach-later like the
	// entry images: the URL lands on promo_slides.image_url through the admin
	// promo form, so the slide row may not exist yet when the art is chosen.
	TypePromoBanner Type = "promo_banner"
	// TypePaymentSlip is a Phase 11 PRIVATE payment-evidence image: the
	// PromptPay slip a reader submits for a Premium payment (docs/MONETIZATION.md,
	// addendum §12). Unlike every other type it is NEVER publicly served - the
	// /media/*key route refuses it (PrivateType), and it is reachable only
	// through the authenticated owner/staff route. It attaches to a
	// subscription payment, not to a user or novel.
	TypePaymentSlip Type = "payment_slip"
)

// ValidType reports whether a value belongs to the media-type vocabulary.
func ValidType(t string) bool {
	switch Type(t) {
	case TypeAvatar, TypeProfileBanner, TypeNovelCover, TypeEntryImage, TypeChapterImage,
		TypeCharacterAvatar, TypeCommunityImage, TypeAttachment, TypePaymentSlip,
		TypePromoBanner:
		return true
	}
	return false
}

// UploadableType reports whether uploads with this purpose are accepted
// TODAY - the types with an owning surface to attach to.
func UploadableType(t string) bool {
	switch Type(t) {
	case TypeAvatar, TypeProfileBanner, TypeNovelCover, TypeEntryImage, TypeChapterImage,
		TypeCharacterAvatar, TypePaymentSlip, TypePromoBanner:
		return true
	}
	return false
}

// UploadableTypes returns the publicly-advertised upload purposes, for clients.
// payment_slip is deliberately absent: it is an internal purpose driven by the
// Premium checkout flow, not a general-purpose upload. promo_banner is absent
// for the same reason - only the staff promo console sends it.
func UploadableTypes() []string {
	return []string{
		"avatar", "profile_banner", "novel_cover",
		"entry_image", "chapter_image", "character_avatar",
	}
}

// PrivateType reports whether media of this purpose must NEVER be served through
// the public /media/*key route. Private media is served only through the
// authenticated, owner/staff route (addendum §9–§11, §14).
func PrivateType(t Type) bool { return t == TypePaymentSlip }

// PrivateMediaURL is the relative path of the authorized serve route for a
// private object. It carries the media id, never the storage key (addendum §14).
// Clients resolve it against the API base.
func PrivateMediaURL(id uuid.UUID) string {
	return "/api/v1/media/" + id.String() + "/private"
}

// imageExtensions maps the SNIFFED content type to the stored extension -
// the allowlist of docs/11 §28. The client's MIME type, extension, and
// filename never participate (docs/09 §27 "Never trust the client-provided
// MIME type alone"). SVG is deliberately absent (script-in-image risk), as
// is everything that is not a raster image.
var imageExtensions = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

// AllowedMimeTypes returns the accepted content types, for clients and docs.
func AllowedMimeTypes() []string { return []string{"image/jpeg", "image/png", "image/webp"} }

// ExtensionFor returns the storage extension for a sniffed content type, or
// "" when the type is not allowlisted.
func ExtensionFor(sniffed string) string { return imageExtensions[sniffed] }

// MaxFilenameRunes bounds the stored original_filename metadata.
const MaxFilenameRunes = 255

// Media mirrors the media table (docs/08 §22.1).
type Media struct {
	ID      uuid.UUID
	OwnerID uuid.UUID

	ObjectKey        string
	OriginalFilename *string
	MimeType         string
	SizeBytes        int64
	MediaType        Type

	CreatedAt time.Time
	DeletedAt *time.Time
}

// View is the API shape of one media object. The object key appears only as
// part of the public URL - clients never learn storage internals beyond the
// path they fetch (docs/11 §29).
type View struct {
	ID               uuid.UUID `json:"id"`
	URL              string    `json:"url"`
	MediaType        Type      `json:"media_type"`
	MimeType         string    `json:"mime_type"`
	SizeBytes        int64     `json:"size_bytes"`
	OriginalFilename *string   `json:"original_filename,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// Render builds the API view. publicURL is the serve URL computed by the
// service from configuration.
func (m *Media) Render(publicURL string) View {
	return View{
		ID:               m.ID,
		URL:              publicURL,
		MediaType:        m.MediaType,
		MimeType:         m.MimeType,
		SizeBytes:        m.SizeBytes,
		OriginalFilename: m.OriginalFilename,
		CreatedAt:        m.CreatedAt,
	}
}

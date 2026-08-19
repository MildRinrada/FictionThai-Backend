package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/platform/storage"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// NovelAccess is the thin slice of the novels domain fiction-scoped uploads
// need. Ownership stays enforced INSIDE the novels service (docs/10 §27):
// ForWriter answers "may this caller change this fiction's settings" (owner
// only - a cover is part of the fiction's presentation the owner controls),
// ForEditor answers "may this caller work on its content" (owner or
// collaborator, 13U - entry/chapter/character images are content work), both
// with the reader-identical 404 for private work. SetCover applies the cover
// reference under the same rule.
type NovelAccess interface {
	ForWriter(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
	ForEditor(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
	SetCover(ctx context.Context, identity *auth.Identity, ref novels.Ref, coverURL *string) error
}

// AvatarStore is the slice of the users repository an avatar upload needs.
// It only ever writes the UPLOADER's own row - there is no cross-user path.
type AvatarStore interface {
	SetAvatarURL(ctx context.Context, userID uuid.UUID, avatarURL *string) error
	SetBannerURL(ctx context.Context, userID uuid.UUID, bannerURL *string) error
}

// PaymentSlipTarget is the sliver of the subscriptions domain a payment-slip
// upload needs (addendum §12–§13). Media owns the bytes and their validation;
// the subscriptions service owns whether the caller may attach a slip to a
// payment and records the reference. Authorization stays THERE (docs/10 §27):
// Authorize runs before any byte is stored, Attach records the media id after.
// nil when the subscriptions feature is not wired - payment_slip uploads are
// then refused.
type PaymentSlipTarget interface {
	AuthorizePaymentSlip(ctx context.Context, ownerID, paymentID uuid.UUID) error
	AttachPaymentSlip(ctx context.Context, ownerID, paymentID, mediaID uuid.UUID) error
}

// Config carries the tunables the service needs (docs/11 §28).
type Config struct {
	// MaxUploadBytes caps one file.
	MaxUploadBytes int64
	// PublicBaseURL is the origin under which /media/{key} is served.
	PublicBaseURL string
}

// Service owns media business rules and is the authorization boundary for
// every media endpoint (docs/10 §27).
type Service struct {
	repo     *Repository
	store    storage.Store
	novels   NovelAccess
	avatars  AvatarStore
	payments PaymentSlipTarget
	cfg      Config
	log      *slog.Logger
}

func NewService(
	repo *Repository, store storage.Store,
	novelAccess NovelAccess, avatarStore AvatarStore, paymentTarget PaymentSlipTarget,
	cfg Config, log *slog.Logger,
) *Service {
	return &Service{
		repo: repo, store: store,
		novels: novelAccess, avatars: avatarStore, payments: paymentTarget,
		cfg: cfg, log: log,
	}
}

func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "MEDIA_NOT_FOUND", "Media not found.")
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

// PublicURL computes the serve URL for an object key.
func (s *Service) PublicURL(key string) string {
	return strings.TrimRight(s.cfg.PublicBaseURL, "/") + "/media/" + key
}

// MaxUploadBytes exposes the cap for the route-level body limit.
func (s *Service) MaxUploadBytes() int64 { return s.cfg.MaxUploadBytes }

// ---------------------------------------------------------------------------
// Upload (docs/09 §27, docs/07 §23, docs/11 §28)
// ---------------------------------------------------------------------------

// UploadInput is one upload request. File is the raw body of the part - read
// and validated HERE; nothing client-declared about it is trusted.
type UploadInput struct {
	// Purpose is the media_type this upload is for.
	Purpose string
	// NovelRef names the fiction a novel_cover attaches to (id or slug).
	NovelRef string
	// PaymentRef names the subscription payment a payment_slip attaches to (id).
	PaymentRef string
	// Filename is the client's original name - sanitized METADATA only, never
	// part of a storage path (docs/11 §28).
	Filename string
	// File is the uploaded bytes.
	File io.Reader
}

// Upload validates, stores, records, and attaches one file, in that order
// (docs/07 §23: storage first, metadata after - a row never references bytes
// that were not safely written). Every step after storage compensates on
// failure so metadata and storage cannot silently diverge.
func (s *Service) Upload(
	ctx context.Context, identity *auth.Identity, input UploadInput,
) (*View, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}

	if !UploadableType(input.Purpose) {
		msg := "Unknown upload purpose."
		if ValidType(input.Purpose) {
			msg = "This upload purpose is not supported yet."
		}
		return nil, apierror.Validation(map[string][]string{"purpose": {msg}})
	}
	mediaType := Type(input.Purpose)

	// Authorize the attachment target BEFORE any byte is stored, so an
	// unauthorized caller cannot make the platform write objects. For covers
	// this is the novels service's own ownership rule - including its
	// non-oracle 404 for someone else's private work (docs/11 §21).
	// Authorize the attachment target BEFORE any byte is stored, so an
	// unauthorized caller cannot make the platform write objects.
	var novelRef novels.Ref
	var paymentID uuid.UUID
	switch mediaType {
	case TypeNovelCover, TypeEntryImage, TypeChapterImage, TypeCharacterAvatar:
		ref, err := novels.ParseRef(strings.TrimSpace(input.NovelRef))
		if err != nil {
			return nil, apierror.Validation(map[string][]string{
				"novel": {"A novel id or slug is required for this upload."},
			})
		}
		novelRef = ref
		// A cover is the owner's presentation setting; the other three are
		// CONTENT work, which a collaborator may also do (13U) - the same split
		// the chapters and characters services enforce.
		gate := s.novels.ForWriter
		if mediaType != TypeNovelCover {
			gate = s.novels.ForEditor
		}
		if _, err := gate(ctx, identity, novelRef); err != nil {
			return nil, err
		}
	case TypePromoBanner:
		// The home hero's slide art (docs/HOME-PROMO.md): staff only, checked
		// before any byte is stored like every other purpose.
		if !identity.IsStaff() {
			return nil, apierror.Forbidden("Staff only.")
		}
	case TypePaymentSlip:
		// Payment slips depend on the subscriptions domain being wired.
		if s.payments == nil {
			return nil, apierror.Validation(map[string][]string{
				"purpose": {"This upload purpose is not supported yet."},
			})
		}
		pid, err := uuid.Parse(strings.TrimSpace(input.PaymentRef))
		if err != nil {
			return nil, apierror.Validation(map[string][]string{
				"payment": {"A valid payment id is required for a payment slip."},
			})
		}
		paymentID = pid
		// Ownership + state ("your pending payment, awaiting evidence") is the
		// subscriptions service's rule, checked here before any byte is written.
		if err := s.payments.AuthorizePaymentSlip(ctx, userID, paymentID); err != nil {
			return nil, err
		}
	}

	contents, sniffed, err := s.readAndValidate(input.File)
	if err != nil {
		return nil, err
	}

	key := string(mediaType) + "/" + uuid.NewString() + ExtensionFor(sniffed)

	if err := s.store.Put(key, bytes.NewReader(contents)); err != nil {
		return nil, s.internal("store object", err)
	}

	record, err := s.repo.Insert(ctx, InsertParams{
		OwnerID:          userID,
		ObjectKey:        key,
		OriginalFilename: sanitizeFilename(input.Filename),
		MimeType:         sniffed,
		SizeBytes:        int64(len(contents)),
		MediaType:        mediaType,
	})
	if err != nil {
		// Compensate: the object must not outlive the failed metadata write.
		if cleanupErr := s.store.Delete(key); cleanupErr != nil {
			s.log.Error("orphaned object after failed media insert",
				slog.String("object_key", key), slog.Any("error", cleanupErr))
		}
		return nil, s.internal("record media", err)
	}

	if err := s.attach(ctx, identity, userID, mediaType, novelRef, paymentID, record); err != nil {
		// Compensate: an unattachable upload is withdrawn entirely.
		if _, delErr := s.repo.SoftDelete(ctx, record.ID); delErr != nil {
			s.log.Error("could not withdraw media after failed attach",
				slog.String("media_id", record.ID.String()), slog.Any("error", delErr))
		}
		if cleanupErr := s.store.Delete(key); cleanupErr != nil {
			s.log.Error("orphaned object after failed attach",
				slog.String("object_key", key), slog.Any("error", cleanupErr))
		}
		return nil, err
	}

	s.log.Info("media uploaded",
		slog.String("media_id", record.ID.String()),
		slog.String("media_type", string(mediaType)),
		slog.String("owner_id", userID.String()),
		slog.Int64("size_bytes", record.SizeBytes),
	)

	view := record.Render(s.serveURL(record))
	return &view, nil
}

// serveURL is the URL a client uses to fetch an object: the public /media path
// for public types, or the private, authorized path for private ones - the
// latter carries the media id, never the storage key (addendum §14).
func (s *Service) serveURL(m *Media) string {
	if PrivateType(m.MediaType) {
		return PrivateMediaURL(m.ID)
	}
	return s.PublicURL(m.ObjectKey)
}

// readAndValidate consumes the upload, enforcing the size cap and the
// signature-based type allowlist (docs/11 §28: never the extension, never the
// client's MIME type).
func (s *Service) readAndValidate(file io.Reader) ([]byte, string, error) {
	if file == nil {
		return nil, "", apierror.Validation(map[string][]string{
			"file": {"A file is required."},
		})
	}

	contents, err := io.ReadAll(io.LimitReader(file, s.cfg.MaxUploadBytes+1))
	if err != nil {
		// The route-level body cap surfaces as a read error mid-body.
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return nil, "", apierror.New(http.StatusRequestEntityTooLarge,
				apierror.CodePayloadTooLarge, "The file is too large.")
		}
		return nil, "", s.internal("read upload", err)
	}
	if int64(len(contents)) > s.cfg.MaxUploadBytes {
		return nil, "", apierror.New(http.StatusRequestEntityTooLarge,
			apierror.CodePayloadTooLarge, "The file is too large.")
	}
	if len(contents) == 0 {
		return nil, "", apierror.Validation(map[string][]string{
			"file": {"A file is required."},
		})
	}

	sniffed := http.DetectContentType(contents)
	if ExtensionFor(sniffed) == "" {
		return nil, "", apierror.Validation(map[string][]string{
			"file": {"Only JPEG, PNG, or WebP images are supported."},
		})
	}
	return contents, sniffed, nil
}

// attach writes the reference into the owning domain. Public types store their
// public URL on the owning row; a payment_slip is referenced by media id in the
// subscriptions domain (addendum §13), never by a URL and never on the user or
// novel row.
func (s *Service) attach(
	ctx context.Context, identity *auth.Identity, userID uuid.UUID,
	mediaType Type, novelRef novels.Ref, paymentID uuid.UUID, record *Media,
) error {
	switch mediaType {
	case TypeAvatar:
		url := s.PublicURL(record.ObjectKey)
		if err := s.avatars.SetAvatarURL(ctx, userID, &url); err != nil {
			return s.internal("attach avatar", err)
		}
		return nil
	case TypeProfileBanner:
		// Same contract as an avatar: the uploader's own profile row, written
		// immediately, no other domain involved.
		url := s.PublicURL(record.ObjectKey)
		if err := s.avatars.SetBannerURL(ctx, userID, &url); err != nil {
			return s.internal("attach profile banner", err)
		}
		return nil
	case TypeNovelCover:
		url := s.PublicURL(record.ObjectKey)
		return s.novels.SetCover(ctx, identity, novelRef, &url)
	case TypeEntryImage, TypeChapterImage, TypeCharacterAvatar:
		// Deliberately nothing. A headcanon entry is not addressable yet when
		// its picture is chosen - it may be a row the writer has not saved - so
		// the reference lands in chapter_entries.image_url with the rest of the
		// topic, on the chapter PATCH the editor sends next (13M). Character
		// portraits follow the same contract into characters.avatar_url via the
		// characters PATCH. Media still owns the bytes; the owning domain still
		// owns the row.
		return nil
	case TypePaymentSlip:
		return s.payments.AttachPaymentSlip(ctx, userID, paymentID, record.ID)
	}
	return nil
}

// sanitizeFilename reduces the client's name to bounded display metadata.
func sanitizeFilename(raw string) *string {
	name := filepath.Base(strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/")))
	if name == "." || name == "/" || name == "" {
		return nil
	}
	if utf8.RuneCountInString(name) > MaxFilenameRunes {
		name = string([]rune(name)[:MaxFilenameRunes])
	}
	return &name
}

// ---------------------------------------------------------------------------
// Serving - the /media/{key} file route
// ---------------------------------------------------------------------------

// Open resolves a serve-path key to its live metadata and bytes. The DATABASE
// row is authoritative: a soft-deleted object 404s here even if a storage
// delete behind it failed (docs/11 §29 - nothing serves storage directly).
func (s *Service) Open(ctx context.Context, key string) (*Media, io.ReadCloser, error) {
	record, err := s.repo.FindLiveByKey(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, notFound()
	}
	if err != nil {
		return nil, nil, s.internal("resolve object", err)
	}

	// A PRIVATE object is never reachable through the public route, even with a
	// known key (addendum §9): payment slips are financial evidence. Same 404 as
	// a missing object - the public route reveals nothing about their existence.
	if PrivateType(record.MediaType) {
		return nil, nil, notFound()
	}

	reader, err := s.store.Open(record.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		// Metadata without bytes: log loudly - this is the divergence the
		// lifecycle is designed to prevent - but answer a plain 404.
		s.log.Error("media row has no stored object",
			slog.String("media_id", record.ID.String()),
			slog.String("object_key", record.ObjectKey))
		return nil, nil, notFound()
	}
	if err != nil {
		return nil, nil, s.internal("open object", err)
	}
	return record, reader, nil
}

// OpenPrivate resolves an object by id for an AUTHORIZED caller: its OWNER, or
// staff performing verification/moderation. Anyone else - and any missing or
// deleted object - is the same non-oracle 404, so a payment slip's existence is
// never confirmed to a stranger (addendum §9–§11, §14). This is the ONLY way a
// private object's bytes are served.
func (s *Service) OpenPrivate(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (*Media, io.ReadCloser, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, nil, err
	}

	record, err := s.repo.Find(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, notFound()
	}
	if err != nil {
		return nil, nil, s.internal("load private media", err)
	}
	// A deleted slip is unreachable, even to its owner (addendum §25).
	if record.DeletedAt != nil {
		return nil, nil, notFound()
	}
	if record.OwnerID != userID && !identity.IsStaff() {
		return nil, nil, notFound()
	}

	reader, err := s.store.Open(record.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		s.log.Error("private media row has no stored object",
			slog.String("media_id", record.ID.String()),
			slog.String("object_key", record.ObjectKey))
		return nil, nil, notFound()
	}
	if err != nil {
		return nil, nil, s.internal("open private object", err)
	}
	return record, reader, nil
}

// ---------------------------------------------------------------------------
// Deletion
// ---------------------------------------------------------------------------

// Delete withdraws a media object: the owner taking a file back, or staff
// acting with the same right every other domain grants them (docs/09 §14.7's
// precedent). Idempotent - deleting a deleted object is a success
// (docs/09 §33).
//
// Order matters: the ROW is deleted first, which instantly unpublishes the
// file (the serve path checks the row), then the object. A failed object
// delete leaves an unreachable orphan and a loud log line, never a reachable
// ghost.
func (s *Service) Delete(ctx context.Context, identity *auth.Identity, id uuid.UUID) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}

	record, err := s.repo.Find(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return s.internal("load media", err)
	}
	if record.OwnerID != userID && !identity.IsStaff() {
		// Media of these types is publicly served, so a 403 confirms nothing
		// the URL did not already reveal.
		return apierror.Forbidden("Only the owner may delete this file.")
	}
	if record.DeletedAt != nil {
		return nil
	}

	if _, err := s.repo.SoftDelete(ctx, record.ID); err != nil {
		return s.internal("delete media", err)
	}
	if err := s.store.Delete(record.ObjectKey); err != nil {
		s.log.Error("orphaned object after media delete",
			slog.String("media_id", record.ID.String()),
			slog.String("object_key", record.ObjectKey),
			slog.Any("error", err))
	}

	s.log.Info("media deleted",
		slog.String("media_id", record.ID.String()),
		slog.String("actor_id", userID.String()),
		slog.Bool("by_owner", record.OwnerID == userID),
	)
	return nil
}

// ---------------------------------------------------------------------------
// Moderation (docs/11 §38 - media is reportable; docs/08 §24)
// ---------------------------------------------------------------------------

// VisibleForViewer answers (by error) whether a media object can be the
// target of a report: it exists and is live. Media of the current types is
// publicly served, so there is no viewer-specific visibility to consult.
func (s *Service) VisibleForViewer(ctx context.Context, _ *auth.Identity, id uuid.UUID) error {
	record, err := s.repo.Find(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return s.internal("load media", err)
	}
	if record.DeletedAt != nil {
		return notFound()
	}
	return nil
}

// ModerateRemove soft-deletes a media object as a moderation action and
// returns its owner (the moderation notification's recipient). Unlike the
// owner's Delete, the stored OBJECT is kept, so a wrongly-removed file can be
// restored intact.
func (s *Service) ModerateRemove(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (uuid.UUID, error) {
	record, err := s.forModeration(ctx, identity, id)
	if err != nil {
		return uuid.Nil, err
	}
	if record.DeletedAt != nil {
		return uuid.Nil, apierror.Conflict("The media is already removed.")
	}
	if _, err := s.repo.SoftDelete(ctx, record.ID); err != nil {
		return uuid.Nil, s.internal("moderate-remove media", err)
	}
	return record.OwnerID, nil
}

// ModerateRestore clears a moderation removal. Like novels, the single
// deleted_at axis cannot distinguish a moderator's removal from the owner's
// own deletion - restoring an owner-deleted row revives metadata whose object
// is already gone, and the serve path answers 404 for it. Staff judgement
// plus the audit trail govern that edge.
func (s *Service) ModerateRestore(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (uuid.UUID, error) {
	record, err := s.forModeration(ctx, identity, id)
	if err != nil {
		return uuid.Nil, err
	}
	if record.DeletedAt == nil {
		return uuid.Nil, apierror.Conflict("The media is not removed.")
	}
	if err := s.repo.Restore(ctx, record.ID); err != nil {
		return uuid.Nil, s.internal("moderate-restore media", err)
	}
	return record.OwnerID, nil
}

func (s *Service) forModeration(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (*Media, error) {
	if !identity.IsStaff() {
		return nil, apierror.Forbidden("You do not have permission to do that.")
	}
	record, err := s.repo.Find(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load media for moderation", err)
	}
	return record, nil
}

// internal logs the real failure and returns the opaque error (docs/11 §67).
func (s *Service) internal(op string, err error) error {
	s.log.Error("media service failure", slog.String("op", op), slog.Any("error", err))
	return apierror.Internal()
}

package chapters

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Revision history (chat-editor review 2026-08, item 10; docs/01 §16).
//
// Snapshots have been written on every content save since the schema existed
// (docs/CONTENT-MODEL.md §5) - this file makes them REACHABLE: a list the
// editor can show, and a restore that brings one back.
//
// Restoring is itself an ordinary content update through Repository.Update,
// which snapshots the chapter as it stands before writing - so a restore can
// always be undone by restoring the version it displaced. Nothing here can
// destroy writer content.

// maxRevisionsListed bounds the history list. Fifty saves is weeks of work;
// paginating further back adds cost to every read for a tail nobody scrolls.
const maxRevisionsListed = 50

// RevisionMeta is one history row: enough to pick a version, nothing more.
type RevisionMeta struct {
	Version      int
	Title        *string
	WordCount    int
	MessageCount int
	EntryCount   int
	CreatedAt    time.Time
}

// RevisionSnapshot is one full snapshot, as stored.
type RevisionSnapshot struct {
	Version       int
	Title         *string
	Content       *string
	ContentFormat string
	// Raw JSON arrays as written ([]MessageView / []EntryView / []string).
	// NULL means the chapter had no such representation at that version.
	Messages    []byte
	Entries     []byte
	EntryFields []byte
	CreatedAt   time.Time
}

// ListRevisions returns a chapter's history, newest first.
func (r *Repository) ListRevisions(ctx context.Context, chapterID uuid.UUID) ([]RevisionMeta, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT version, title, word_count,
		       COALESCE(jsonb_array_length(messages), 0),
		       COALESCE(jsonb_array_length(entries), 0),
		       created_at
		FROM chapter_revisions
		WHERE chapter_id = $1
		ORDER BY version DESC
		LIMIT $2`, chapterID, maxRevisionsListed)
	if err != nil {
		return nil, fmt.Errorf("list revisions: %w", err)
	}
	defer rows.Close()

	list := []RevisionMeta{}
	for rows.Next() {
		var meta RevisionMeta
		if err := rows.Scan(&meta.Version, &meta.Title, &meta.WordCount,
			&meta.MessageCount, &meta.EntryCount, &meta.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan revision: %w", err)
		}
		list = append(list, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list revisions: %w", err)
	}
	return list, nil
}

// FindRevision loads one snapshot.
func (r *Repository) FindRevision(
	ctx context.Context, chapterID uuid.UUID, version int,
) (*RevisionSnapshot, error) {
	var snapshot RevisionSnapshot
	err := r.db.QueryRowContext(ctx, `
		SELECT version, title, content, COALESCE(content_format, 'plain'),
		       messages, entries, entry_fields, created_at
		FROM chapter_revisions
		WHERE chapter_id = $1 AND version = $2`, chapterID, version).
		Scan(&snapshot.Version, &snapshot.Title, &snapshot.Content,
			&snapshot.ContentFormat, &snapshot.Messages, &snapshot.Entries,
			&snapshot.EntryFields, &snapshot.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find revision: %w", err)
	}
	return &snapshot, nil
}

// RevisionView is one history row as the API returns it.
type RevisionView struct {
	Version      int       `json:"version"`
	Title        *string   `json:"title,omitempty"`
	WordCount    int       `json:"word_count"`
	MessageCount int       `json:"message_count"`
	EntryCount   int       `json:"entry_count"`
	CreatedAt    time.Time `json:"created_at"`
}

// Revisions lists a chapter's history. Editors only - history is the writing
// room, not the reading room.
func (s *Service) Revisions(
	ctx context.Context, identity *auth.Identity, novelRef novels.Ref, chapterRef Ref,
) ([]RevisionView, error) {
	novel, err := s.novels.ForEditor(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}
	chapter, err := s.repo.Find(ctx, novel.ID, chapterRef)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load chapter", err)
	}

	list, err := s.repo.ListRevisions(ctx, chapter.ID)
	if err != nil {
		return nil, s.internal("list revisions", err)
	}
	views := make([]RevisionView, 0, len(list))
	for _, meta := range list {
		views = append(views, RevisionView(meta))
	}
	return views, nil
}

// RestoreRevision brings a snapshot back as the chapter's current content.
//
// Every content field is set EXPLICITLY - title, prose, messages, entries,
// entry fields, content format - because a snapshot is the complete authored
// state (docs/CONTENT-MODEL.md §5), and restoring half of one would stitch two
// versions into a chapter the author never wrote. Status and schedule are left
// alone: bringing back last week's text must not unpublish this week's chapter.
func (s *Service) RestoreRevision(
	ctx context.Context, identity *auth.Identity,
	novelRef novels.Ref, chapterRef Ref, version int,
) (*View, error) {
	novel, err := s.novels.ForEditor(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}
	chapter, err := s.repo.Find(ctx, novel.ID, chapterRef)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load chapter", err)
	}

	snapshot, err := s.repo.FindRevision(ctx, chapter.ID, version)
	if errors.Is(err, ErrNotFound) {
		return nil, apierror.New(http.StatusNotFound, "REVISION_NOT_FOUND", "Revision not found.")
	}
	if err != nil {
		return nil, s.internal("load revision", err)
	}

	messages, err := messagesFromSnapshot(snapshot.Messages)
	if err != nil {
		return nil, s.internal("decode revision messages", err)
	}
	entries, fields, err := entriesFromSnapshot(snapshot.Entries, snapshot.EntryFields)
	if err != nil {
		return nil, s.internal("decode revision entries", err)
	}

	// A character deleted since the snapshot would leave an entry pointing at
	// nothing (an FK violation on insert). The name is denormalised beside the
	// id for exactly this case, so the link is dropped and the entry survives.
	if err := s.dropMissingEntryCharacters(ctx, novel.ID, entries); err != nil {
		return nil, err
	}

	format := ContentFormat(snapshot.ContentFormat)
	params := UpdateParams{
		ChapterID:     chapter.ID,
		ActorID:       identity.UserID(),
		Title:         &snapshot.Title,
		Content:       &snapshot.Content,
		Messages:      &messages,
		Entries:       &entries,
		EntryFields:   &fields,
		ContentFormat: &format,
	}

	updated, err := s.repo.Update(ctx, params)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("restore revision", err)
	}
	return s.ownerView(ctx, novel, updated)
}

// messagesFromSnapshot rebuilds the conversation a snapshot recorded.
//
// Deliberately NOT revalidated against today's limits: the snapshot was valid
// when it was written, and a tightened limit must never strand an author's own
// history out of reach.
func messagesFromSnapshot(raw []byte) ([]Message, error) {
	if len(raw) == 0 {
		return []Message{}, nil
	}
	var views []MessageView
	if err := json.Unmarshal(raw, &views); err != nil {
		return nil, fmt.Errorf("decode message snapshot: %w", err)
	}
	messages := make([]Message, 0, len(views))
	for at, view := range views {
		messages = append(messages, Message{
			Position:         at,
			SpeakerName:      view.SpeakerName,
			SpeakerAvatarURL: view.SpeakerAvatarURL,
			Type:             view.MessageType,
			Content:          view.Content,
			Metadata:         view.Metadata,
		})
	}
	return messages, nil
}

// entriesFromSnapshot rebuilds the headcanon representation a snapshot recorded.
func entriesFromSnapshot(rawEntries, rawFields []byte) ([]Entry, []string, error) {
	entries := []Entry{}
	if len(rawEntries) > 0 {
		var views []EntryView
		if err := json.Unmarshal(rawEntries, &views); err != nil {
			return nil, nil, fmt.Errorf("decode entry snapshot: %w", err)
		}
		for at, view := range views {
			entries = append(entries, Entry{
				Position:    at,
				CharacterID: view.CharacterID,
				Name:        view.Name,
				Values:      view.Values,
				Body:        view.Body,
				ImageURL:    view.ImageURL,
			})
		}
	}

	fields := []string{}
	if len(rawFields) > 0 {
		if err := json.Unmarshal(rawFields, &fields); err != nil {
			return nil, nil, fmt.Errorf("decode entry fields snapshot: %w", err)
		}
	}
	return entries, fields, nil
}

// dropMissingEntryCharacters unlinks entries whose character no longer exists.
func (s *Service) dropMissingEntryCharacters(
	ctx context.Context, novelID uuid.UUID, entries []Entry,
) error {
	ids := make([]uuid.UUID, 0, len(entries))
	for _, entry := range entries {
		if entry.CharacterID != nil {
			ids = append(ids, *entry.CharacterID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	found, err := s.repo.CharactersInNovel(ctx, novelID, ids)
	if err != nil {
		return s.internal("check revision characters", err)
	}
	for i := range entries {
		if entries[i].CharacterID != nil && !found[*entries[i].CharacterID] {
			entries[i].CharacterID = nil
		}
	}
	return nil
}

// Revisions handles GET /api/v1/novels/:novel/chapters/:chapter/revisions.
func (h *Handler) Revisions(c *gin.Context) {
	novelRef, chapterRef, ok := refsFrom(c)
	if !ok {
		return
	}
	views, err := h.service.Revisions(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), novelRef, chapterRef)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, views)
}

// RestoreRevision handles POST
// /api/v1/novels/:novel/chapters/:chapter/revisions/:version/restore.
func (h *Handler) RestoreRevision(c *gin.Context) {
	novelRef, chapterRef, ok := refsFrom(c)
	if !ok {
		return
	}
	version, err := strconv.Atoi(c.Param("version"))
	if err != nil || version < 1 {
		response.Fail(c, apierror.New(http.StatusNotFound, "REVISION_NOT_FOUND", "Revision not found."))
		return
	}
	view, serviceErr := h.service.RestoreRevision(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), novelRef, chapterRef, version)
	if serviceErr != nil {
		response.Fail(c, serviceErr)
		return
	}
	response.OK(c, view)
}

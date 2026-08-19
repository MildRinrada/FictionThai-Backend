package achievements

import (
	"context"

	"github.com/google/uuid"
)

// The signal side, as the domains see it.
//
// Every method here is named after the MOMENT it belongs to, not the
// achievement it feeds. That is the notifications.Service pattern and it is
// what keeps the catalogue keys - and the thresholds that go with them - in
// this package: a domain reports what happened in its own vocabulary and never
// learns that "first_chapter" is a string, or that ปิดจบ wants three chapters.
//
// Each domain declares its own one- or two-method interface (chapters.Achiever,
// novels.Achiever, characters.Achiever, ai.Achiever) and this service satisfies
// all of them, so the dependency runs one way: nothing imports this package
// except the wiring in cmd/server.
//
// All of it is FIRE-AND-FORGET. The writer's action has already committed by
// the time any of these is called; a failure is logged and never surfaces.

// completedMinChapters is the floor under ปิดจบ. Marking a two-chapter draft
// "completed" is a status change, not a finished story - and the achievement
// exists to mean the second thing.
const completedMinChapters = 3

// ChapterPublished satisfies chapters.Achiever: a chapter went live.
//
// Called on EVERY first publish rather than only the writer's first ever,
// because the service already knows which of those is the first: เริ่มต้น has
// a threshold of one and an idempotent unlock, so the second call and the two
// hundredth are both no-ops.
func (s *Service) ChapterPublished(ctx context.Context, authorID uuid.UUID) {
	s.Record(ctx, authorID, KeyFirstChapter, Options{})
}

// FictionCompleted satisfies novels.Achiever: a fiction was moved to
// "จบแล้ว". The chapter count travels with it because the RULE about how long
// a finished story has to be belongs here, not in the novels service.
func (s *Service) FictionCompleted(ctx context.Context, authorID uuid.UUID, chapterCount int) {
	if chapterCount < completedMinChapters {
		return
	}
	s.Record(ctx, authorID, KeyCompleted, Options{})
}

// CharacterDetailed satisfies characters.Achiever: a cast member was created
// with more on their sheet than a name. Whether a sheet has anything on it is
// a fact about the characters domain's own data, so that domain decides it;
// how many such sheets make a นักสร้างโลก is decided here.
func (s *Service) CharacterDetailed(ctx context.Context, authorID uuid.UUID) {
	s.Record(ctx, authorID, KeyWorldbuilder, Options{})
}

// SuggestionAccepted satisfies ai.Achiever: the writer took one of the
// assistant's suggestions. Accepting never edits the manuscript (docs/12 §15),
// so this counts a DECISION, not a change to anybody's words.
func (s *Service) SuggestionAccepted(ctx context.Context, userID uuid.UUID) {
	s.Record(ctx, userID, KeyNativeSpeaker, Options{})
}

// SuggestionMuted satisfies ai.Achiever: the writer taught the assistant to
// stop ("ไม่เตือนแบบนี้อีก"). ไม่เชื่อ AI is deliberately as earnable as
// เจ้าของภาษา - disagreeing with the assistant is a legitimate way to write
// here, and the achievement set says so.
func (s *Service) SuggestionMuted(ctx context.Context, userID uuid.UUID) {
	s.Record(ctx, userID, KeyTrustNoAI, Options{})
}

// ChapterRead satisfies a reader-driven signal: somebody OTHER than the writer
// opened their work. readerID is the whole anti-farming mechanism - distinct
// accounts, each older than seven days - so a caller with no signed-in reader
// passes uuid.Nil and this counts nothing.
func (s *Service) ChapterRead(ctx context.Context, authorID, readerID uuid.UUID) {
	s.Record(ctx, authorID, KeyFirstReader, Options{ActorID: readerID})
}

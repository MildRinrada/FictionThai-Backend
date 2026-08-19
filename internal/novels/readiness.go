package novels

import (
	"strings"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// The pre-publish checklist - the second gate
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13L).
//
// 13A was right to cut the create form to six fields, but it left the synopsis,
// the genres, and the tags with no point at which they were ever required
// again. The result was a lighter form that produced worse data.
//
// This is where they come back: not as questions before the first sentence, but
// as a gate in front of PUBLISHING - the moment the work stops being the
// writer's own business and starts being something readers have to find,
// classify, and decide about.
//
// Every item earns its place by being useless to collect earlier and impossible
// to do without later:
//
//	เรื่องย่อ    a reader decides from it; a card without one is a blank
//	หมวดหมู่     the only way the work appears in a browse surface at all
//	แท็ก        how readers who want exactly this find it
//	คำเตือน      only for 15+/18+ - see below
//	ปก          every listing renders one; the fallback is the same grey box
//	ยืนยันอีเมล   already the rule (docs/AUTHENTICATION.md §9), now stated here too
//
// It is a gate on publishing ONLY. Drafting, editing, adding chapters, and
// keeping a private work forever are all untouched: nothing here may ever stop
// a writer from writing.

// ReadinessItem is one entry on the checklist.
type ReadinessItem struct {
	// Key is stable and machine-readable, so the client can link each item to
	// the field that satisfies it rather than matching on a message.
	Key string `json:"key"`
	// Label is what the writer reads.
	Label string `json:"label"`
	Done  bool   `json:"done"`
	// Hint says what to do about it, and appears only when it is not done.
	Hint string `json:"hint,omitempty"`
	// Required separates the gate from the advice. A required item blocks the
	// publish; a recommended one is stated as worth doing and never blocks.
	// The split exists because a checklist where all five rows look equally
	// mandatory cannot answer "what is actually left before I can publish".
	Required bool `json:"required"`
}

// Readiness is the whole checklist plus the one answer that matters.
type Readiness struct {
	Items []ReadinessItem `json:"items"`
	// Ready reports whether every REQUIRED item is done. The API refuses to
	// publish when it is false, so the client never has to decide
	// (docs/11 §43). Recommended items never hold it false.
	Ready bool `json:"ready"`
}

// CheckReadiness builds the checklist for a fiction.
//
// It takes the identity because email verification is one of the items, and a
// checklist that omitted it would be telling the writer they are ready when the
// publish will be refused.
func CheckReadiness(novel *Novel, genres, tags int, identity *auth.Identity) Readiness {
	items := []ReadinessItem{
		{
			Key:      "description",
			Label:    "เรื่องย่อ",
			Done:     novel.Description != nil && strings.TrimSpace(*novel.Description) != "",
			Hint:     "ผู้อ่านตัดสินใจจากเรื่องย่อ - การ์ดที่ไม่มีเรื่องย่อคือการ์ดเปล่า",
			Required: true,
		},
		{
			Key:      "genres",
			Label:    "หมวดหมู่",
			Done:     genres > 0,
			Hint:     "ถ้าไม่เลือก เรื่องจะไม่ไปโผล่ในหน้าหมวดไหนเลย",
			Required: true,
		},
		{
			Key:      "tags",
			Label:    "แท็ก",
			Done:     tags > 0,
			Hint:     "แท็กคือทางที่คนที่อยากอ่านเรื่องแบบนี้พอดีจะเจอคุณ",
			Required: true,
		},
		{
			// Recommended, not required: a good cover helps the card, but a
			// finished story with no cover yet is still publishable work, and
			// the grey-box fallback exists precisely for it.
			Key:   "cover",
			Label: "ปกเรื่อง",
			Done:  novel.CoverURL != nil && strings.TrimSpace(*novel.CoverURL) != "",
			Hint:  "ทุกหน้ารวมแสดงปก ถ้าไม่มีจะเป็นกล่องเทาเหมือนกันหมด",
		},
	}

	// The content warning is required only where it means something. Demanding
	// one on a ทั่วไป fiction would teach writers to type "ไม่มี" to get past a
	// gate, which is worse than no field at all.
	if novel.AgeRating == RatingTeen || novel.AgeRating.Adult() {
		items = append(items, ReadinessItem{
			Key:      "content_warning",
			Label:    "คำเตือนเนื้อหา",
			Done:     novel.ContentWarning != nil && strings.TrimSpace(*novel.ContentWarning) != "",
			Hint:     "เรตนี้ต้องบอกล่วงหน้าว่ามีอะไร - ผู้อ่านกลุ่มนี้ต้องการมันจริง ๆ",
			Required: true,
		})
	}

	// The author's own adult statement, asked once per account (§13B). It
	// appears on the checklist only for the work it applies to, so a writer who
	// never publishes 18+ never sees the question at all.
	if novel.AgeRating.Adult() {
		items = append(items, ReadinessItem{
			Key:   "adult_attested",
			Label: "ยืนยันว่าคุณอายุ 18 ปีขึ้นไป",
			Done: identity.IsStaff() ||
				(identity.Authenticated() && identity.User.AdultAttested()),
			Hint:     "ยืนยันครั้งเดียวที่หน้าโปรไฟล์ ใช้ได้กับทุกเรื่องหลังจากนั้น",
			Required: true,
		})
	}

	items = append(items, ReadinessItem{
		Key:      "email_verified",
		Label:    "ยืนยันอีเมล",
		Done:     identity.EmailVerified() || identity.IsStaff(),
		Hint:     "ยืนยันอีเมลก่อนเผยแพร่ครั้งแรก",
		Required: true,
	})

	ready := true
	for _, item := range items {
		if item.Required && !item.Done {
			ready = false
		}
	}
	return Readiness{Items: items, Ready: ready}
}

// requireReadyToPublish refuses a publish that the checklist is not satisfied
// for.
//
// The error names the FIELDS, so the form can point at each one rather than
// showing a paragraph the writer has to decode. It is a 422 rather than a 403:
// nothing is forbidden here, the request is simply incomplete.
func requireReadyToPublish(readiness Readiness) error {
	if readiness.Ready {
		return nil
	}

	// Only the REQUIRED gaps become errors. A recommended item that is not
	// done is exactly the thing this error must not mention: naming it here
	// would teach the writer that "recommended" was a lie.
	fields := map[string][]string{}
	for _, item := range readiness.Items {
		if item.Required && !item.Done {
			fields[item.Key] = []string{item.Hint}
		}
	}

	err := apierror.Validation(fields)
	err.Message = "ยังเผยแพร่ไม่ได้ - ทำรายการที่เหลือให้ครบก่อน"
	return err
}

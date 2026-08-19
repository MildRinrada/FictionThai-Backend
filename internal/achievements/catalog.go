// Package achievements owns the award, the tally, and the switch that turns
// the whole thing off (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3).
//
// Three families doing three different jobs:
//
//	เส้นทาง (path)      tells a new writer what to do next; doubles as
//	                    onboarding. Listed openly, with progress.
//	ตัวตน (identity)    says what kind of writer you are. Listed openly.
//	Easter egg          found, never announced. NEVER listed.
//
// The rules that are easy to get wrong, and are therefore enforced here rather
// than left to a caller's good manners:
//
//   - No points, no total score, no leaderboard. There is nothing in this
//     package that produces a number two people could be sorted by.
//   - No achievement rewards posting volume. Every counter is either a
//     one-shot fact or a count of DIFFERENT things (modes, characters,
//     decisions) - never "how much did you post".
//   - Every counter carries a per-key cooldown, so ten create-delete cycles in
//     a minute count once.
//   - Anything that counts READERS counts distinct accounts older than 7 days.
//   - An easter egg is never named to anyone but the person who found it.
//
// Signals arrive from the domains that already own the moment - one
// fire-and-forget line at a choke point that exists - and from the browser for
// the four cosmetic eggs, through a server-side allowlist.
package achievements

import "time"

// Family is what an achievement is FOR. It decides how the achievement may be
// displayed, which is why it is part of the catalogue rather than a rendering
// choice made per surface.
type Family string

const (
	// FamilyPath is เส้นทาง: the onboarding path, listed openly with progress.
	FamilyPath Family = "path"
	// FamilyIdentity is ตัวตน: what kind of writer you are. Listed openly.
	FamilyIdentity Family = "identity"
	// FamilyEgg is an easter egg. Never listed, never named to anybody but the
	// person who found it: naming one, or describing how to get it, kills it
	// instantly (Part 3 "The easter-egg rule").
	FamilyEgg Family = "egg"
)

// Definition is one achievement, whole. The Thai copy lives beside the
// threshold that earns it so the two can never drift apart.
type Definition struct {
	// Key is the stable identifier stored in achievements.key.
	Key string
	// Family decides display rules, not importance.
	Family Family
	// Title is what the owner (and, for path/identity, a visitor) reads.
	Title string
	// Description says what earns it, in the writer's language. For an egg
	// this is deliberately empty in every public shape - see Trigger.
	Description string
	// Threshold is how many ACCEPTED signals unlock it. One means the signal
	// itself is the whole event.
	Threshold int
	// Cooldown is the minimum gap between two accepted signals for this key.
	// It is the anti-farming floor: ten create-delete cycles in a minute count
	// once (Part 3 "Anti-farming").
	Cooldown time.Duration
	// ClientTriggerable allows POST /achievements/signal to record this key.
	// The allowlist IS this field: a key without it can never be unlocked from
	// a browser, so no signal from a browser can unlock anything that implies
	// real work.
	ClientTriggerable bool
	// ClientCount is how many times the BROWSER must observe the trigger
	// before it sends its one signal ("a disabled button pressed 20 times").
	// It is a property of the client-side trigger, not of server-side
	// accumulation: the count is unverifiable by definition, and spending
	// twenty requests of a writer's rate-limit budget on a joke would be worse
	// than trusting it. Zero for everything that is not client-triggerable.
	ClientCount int
	// DistinctActors marks a reader-driven achievement: it counts DIFFERENT
	// accounts, each older than 7 days, rather than repeat visits from one
	// person - or from an account made to applaud its owner.
	DistinctActors bool
	// Trigger is what the owner is told about HOW they found an egg, so they
	// can tell somebody. Empty for path and identity, whose Description
	// already says it.
	Trigger string
	// Message is the egg's own words. Warm, aware, never sarcastic, never
	// threatening, no game vocabulary. The security-adjacent ones read as "we
	// noticed, nice try, here is how to report something real" - never as a
	// warning (Part 3 "Tone").
	Message string
}

// Unlocked reports whether count meets this definition's threshold.
func (d Definition) Unlocked(count int) bool { return count >= d.Threshold }

// Catalog is every achievement the platform ships, in display order.
//
// The starter set of docs/PROFILE-AND-ACHIEVEMENTS.md Part 3: เส้นทาง entire,
// because it is the onboarding path; the system-poking four eggs, because they
// are the cheapest to build and the most talked about; and the two ตัวตน
// achievements the platform will be remembered for.
var Catalog = []Definition{
	// ---------------------------------------------------------------------
	// เส้นทาง - the onboarding path. Every one of these is a first, a
	// finish, or a count of DIFFERENT things. None of them counts output.
	// ---------------------------------------------------------------------
	{
		Key:         KeyFirstChapter,
		Family:      FamilyPath,
		Title:       "เริ่มต้น",
		Description: "เผยแพร่ตอนแรกของคุณ",
		Threshold:   1,
		Cooldown:    time.Minute,
	},
	{
		Key:         KeyFirstReader,
		Family:      FamilyPath,
		Title:       "มีคนอ่านจริง ๆ",
		Description: "มีคนอื่นเปิดอ่านงานของคุณ",
		Threshold:   1,
		// Reader-driven: the cooldown is not what protects this one, the
		// distinct-account rule is.
		Cooldown:       0,
		DistinctActors: true,
	},
	{
		Key:         KeyCompleted,
		Family:      FamilyPath,
		Title:       "ปิดจบ",
		Description: "เปลี่ยนสถานะเรื่องที่มีอย่างน้อย 3 ตอนเป็น «จบแล้ว»",
		Threshold:   1,
		Cooldown:    time.Minute,
	},
	{
		Key:         KeyManyVoices,
		Family:      FamilyPath,
		Title:       "นักเล่าหลายเสียง",
		Description: "ใช้ทั้งสามโหมดในเรื่องเดียว",
		Threshold:   1,
		Cooldown:    time.Minute,
	},
	{
		Key:         KeyWorldbuilder,
		Family:      FamilyPath,
		Title:       "นักสร้างโลก",
		Description: "เขียนตัวละคร 10 ตัวที่มีมากกว่าแค่ชื่อ",
		Threshold:   10,
		// A minute, so ten create-delete cycles in a minute count once.
		Cooldown: time.Minute,
	},
	{
		Key:         KeyNativeSpeaker,
		Family:      FamilyPath,
		Title:       "เจ้าของภาษา",
		Description: "รับคำแนะนำจากผู้ช่วยเขียน 100 ครั้ง",
		Threshold:   100,
		// Short: accepting suggestions is real, continuous editing work, and a
		// minute-long gate would make the achievement about waiting.
		Cooldown: 5 * time.Second,
	},
	{
		Key:         KeyTrustNoAI,
		Family:      FamilyPath,
		Title:       "ไม่เชื่อ AI",
		Description: "กด «ไม่เตือนแบบนี้อีก» 50 ครั้ง",
		Threshold:   50,
		Cooldown:    5 * time.Second,
	},

	// ---------------------------------------------------------------------
	// ตัวตน - what kind of writer you are.
	// ---------------------------------------------------------------------
	{
		Key:         KeyOneIsEnough,
		Family:      FamilyIdentity,
		Title:       "หนึ่งคนก็พอ",
		Description: "มีผู้อ่านคนหนึ่งที่คอมเมนต์ครบทุกตอนของคุณ",
		Threshold:   1,
		Cooldown:    0,
		// The whole point is that it is ONE real person, so the account has to
		// be a real one.
		DistinctActors: true,
	},
	{
		Key:         KeyBackAgain,
		Family:      FamilyIdentity,
		Title:       "กลับมาแล้ว",
		Description: "ลงตอนใหม่ในเรื่องที่เงียบไปนานกว่าหนึ่งปี",
		Threshold:   1,
		Cooldown:    time.Minute,
	},

	// ---------------------------------------------------------------------
	// Easter eggs. Cosmetic by definition - each one records that a browser
	// says something happened, and nothing more. None of them can imply work.
	// ---------------------------------------------------------------------
	{
		Key:               KeyEggDevTools,
		Family:            FamilyEgg,
		Title:             "สวัสดีนักสำรวจ",
		Threshold:         1,
		Cooldown:          10 * time.Second,
		ClientTriggerable: true,
		ClientCount:       1,
		Trigger:           "เปิดเครื่องมือนักพัฒนา (DevTools) ในหน้าเว็บของเรา",
		Message: "สวัสดี ยินดีที่ได้เจอคนชอบรื้อดูข้างใน " +
			"ถ้าเจออะไรที่ดูไม่ถูกต้อง บอกเราที่หน้าติดต่อได้เลย เราอ่านทุกฉบับจริง ๆ",
	},
	{
		Key:               KeyEggAdminPath,
		Family:            FamilyEgg,
		Title:             "Nice try",
		Threshold:         1,
		Cooldown:          10 * time.Second,
		ClientTriggerable: true,
		ClientCount:       1,
		Trigger:           "ลองเปิด /admin, /wp-admin หรือ /.env",
		Message: "ตรงนั้นไม่มีอะไรจริง ๆ นะ " +
			"แต่ถ้าคุณเจอช่องโหว่ของจริง เราอยากรู้มาก ส่งมาที่หน้าติดต่อได้เลย",
	},
	{
		Key:               KeyEggDisabledButton,
		Family:            FamilyEgg,
		Title:             "เอาจริงดิ",
		Threshold:         1,
		Cooldown:          10 * time.Second,
		ClientTriggerable: true,
		ClientCount:       20,
		Trigger:           "กดปุ่มที่กดไม่ได้ 20 ครั้ง",
		Message: "ปุ่มนั้นยังกดไม่ได้จริง ๆ ขอโทษที่ไม่ได้บอกให้ชัดกว่านี้ " +
			"ถ้าตรงไหนควรอธิบายมากกว่านี้ บอกเราได้เสมอ",
	},
	{
		Key:               KeyEggCtrlS,
		Family:            FamilyEgg,
		Title:             "Ctrl+S",
		Threshold:         1,
		Cooldown:          10 * time.Second,
		ClientTriggerable: true,
		ClientCount:       30,
		Trigger:           "กด Ctrl+S 30 ครั้งในหน้าที่บันทึกให้อัตโนมัติอยู่แล้ว",
		Message: "งานของคุณถูกบันทึกไว้ตลอดอยู่แล้ว ไม่ต้องกดก็ได้ " +
			"แต่เราเข้าใจนิสัยนี้ดี เราก็กด",
	},
}

// Catalogue keys. Named constants because they are written into a durable
// table and read by the client allowlist - a typo in a string literal would be
// an award nobody can ever earn.
const (
	KeyFirstChapter  = "first_chapter"
	KeyFirstReader   = "first_reader"
	KeyCompleted     = "completed"
	KeyManyVoices    = "many_voices"
	KeyWorldbuilder  = "worldbuilder"
	KeyNativeSpeaker = "native_speaker"
	KeyTrustNoAI     = "trust_no_ai"

	KeyOneIsEnough = "one_is_enough"
	KeyBackAgain   = "back_again"

	KeyEggDevTools       = "egg_devtools"
	KeyEggAdminPath      = "egg_admin_path"
	KeyEggDisabledButton = "egg_disabled_button"
	KeyEggCtrlS          = "egg_ctrl_s"
)

// distinctActorMinAge is how old an account must be before its actions can
// earn somebody else an achievement. Seven days, per Part 3: a fresh account
// applauding its own owner is the farm this rule exists to stop.
const distinctActorMinAge = 7 * 24 * time.Hour

// catalogIndex is the by-key lookup, built once.
var catalogIndex = func() map[string]Definition {
	index := make(map[string]Definition, len(Catalog))
	for _, definition := range Catalog {
		index[definition.Key] = definition
	}
	return index
}()

// Lookup returns a catalogue entry. The second result is false for a key that
// is not shipped - including one retired from the catalogue whose award rows
// are still standing, because a retired achievement is not deleted from
// anybody's history.
func Lookup(key string) (Definition, bool) {
	definition, ok := catalogIndex[key]
	return definition, ok
}

// ClientTriggerable is the server-side allowlist POST /achievements/signal
// checks. It is derived from the catalogue rather than written out separately,
// so a key can never be allowlisted by accident.
func ClientTriggerable(key string) bool {
	definition, ok := Lookup(key)
	return ok && definition.ClientTriggerable
}

// EggTotal is how many eggs exist - the denominator of "ปลดล็อกแล้ว 3 / ??".
// The count is public; the names are not.
func EggTotal() int {
	total := 0
	for _, definition := range Catalog {
		if definition.Family == FamilyEgg {
			total++
		}
	}
	return total
}

// ListedTotal is how many achievements may be listed openly - everything that
// is not an egg. It is the denominator of the medal grid.
func ListedTotal() int { return len(Catalog) - EggTotal() }

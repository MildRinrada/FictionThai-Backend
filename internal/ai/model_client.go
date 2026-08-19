package ai

// The model tier of ตรวจความสอดคล้องของตัวละคร
// (docs/AI-CONSISTENCY-MODEL.md): a local inference sidecar scoring
// (character profile, manuscript line) pairs for semantic contradiction.
//
// The client is deliberately dumb and fail-quiet: the sidecar being absent,
// slow, or broken degrades the check to the deterministic rule floor - the
// writer sees rule findings, never an error. Nothing here retains text.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// modelTimeout bounds one sidecar call. The character round rides the typing
// pause, so a stall here would hold the panel's verdict hostage.
const modelTimeout = 8 * time.Second

// modelMaxPairs caps how many (character, line) pairs one check sends - the
// sidecar has its own cap too; this keeps giant chapters cheap.
const modelMaxPairs = 40

// conflictThreshold is the contradiction probability at which a model finding
// is worth the writer's attention. Below it, silence.
const conflictThreshold = 0.70

// maxModelFindings caps model findings per check, mirroring the per-paragraph
// cap of the live pass: the panel advises, it must not flood.
const maxModelFindings = 3

// modelMinRunes is the shortest line worth scoring. A bare exclamation
// ("แคว่ก!", «หืม?») shares no wording with any character sheet, which an NLI
// model reads as contradiction - a confident finding with nothing behind it.
const modelMinRunes = 24

// ModelPair is one attributed line to score against one character's profile.
type ModelPair struct {
	CharacterID string `json:"character_id"`
	Name        string `json:"name"`
	Profile     string `json:"profile"`
	Line        string `json:"line"`
}

// ModelLabel is one zero-shot behavior/emotion label the sidecar detected.
type ModelLabel struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}

// ModelResult is the sidecar's verdict for one pair.
type ModelResult struct {
	CharacterID   string       `json:"character_id"`
	Line          string       `json:"line"`
	Similarity    float64      `json:"similarity"`
	Contradiction float64      `json:"contradiction"`
	Labels        []ModelLabel `json:"labels"`
	// ProfileLabels is the tone of the character SHEET, read the same way.
	ProfileLabels []ModelLabel `json:"profile_labels"`
}

// toneOpposites pairs the tones that genuinely clash. A model finding must
// name one of these clashes: the contradiction score alone saturates near
// 1.00 over a real chapter and flagged «จริงจัง» lines on a สุขุม character -
// the very tone their sheet asked for. Symmetric; built once at init.
var toneOpposites = map[string]map[string]bool{}

func init() {
	pairs := map[string][]string{
		"ใจเย็น":     {"โกรธ", "ก้าวร้าว", "ตื่นตระหนก", "ตื่นเต้น", "ร่าเริง"},
		"จริงจัง":    {"ขี้เล่น", "ร่าเริง", "ตื่นเต้น"},
		"เย็นชา":     {"ร่าเริง", "ขี้เล่น", "อ่อนโยน", "ตื่นเต้น"},
		"สุภาพ":      {"หยาบคาย", "ก้าวร้าว", "โกรธ"},
		"อ่อนโยน":    {"ก้าวร้าว", "หยาบคาย", "โกรธ", "เย็นชา"},
		"มั่นใจ":     {"เขินอาย", "ตื่นตระหนก"},
		"ร่าเริง":    {"เศร้า"},
		"ขี้เล่น":    {"เศร้า"},
		"ตื่นตระหนก": {"เศร้า"},
	}
	add := func(a, b string) {
		if toneOpposites[a] == nil {
			toneOpposites[a] = map[string]bool{}
		}
		toneOpposites[a][b] = true
	}
	for tone, opposites := range pairs {
		for _, opposite := range opposites {
			add(tone, opposite)
			add(opposite, tone)
		}
	}
}

// clashingTones returns the sheet tone and the line tone that contradict each
// other, or ok=false when the two readings do not actually clash - in which
// case there is nothing worth asking the writer about.
func clashingTones(profile, line []ModelLabel) (sheet, said string, ok bool) {
	for _, lineTone := range line {
		for _, sheetTone := range profile {
			if toneOpposites[sheetTone.Label][lineTone.Label] {
				return sheetTone.Label, lineTone.Label, true
			}
		}
	}
	return "", "", false
}

// ModelClient talks to the consistency sidecar. A nil *ModelClient is valid
// and means "no model tier configured".
type ModelClient struct {
	baseURL string
	http    *http.Client
}

// NewModelClient returns nil when url is empty - callers treat nil as
// "rules only", which is the default deployment.
func NewModelClient(url string) *ModelClient {
	if url == "" {
		return nil
	}
	return &ModelClient{
		baseURL: url,
		http:    &http.Client{Timeout: modelTimeout},
	}
}

// Consistency scores the pairs. It also returns how many of them the sidecar
// has queued but not scored yet (it scores asynchronously) - the caller
// surfaces that so the panel knows to ask again. Any transport or decoding
// failure returns an error the caller LOGS AND DROPS - the rule floor
// already answered.
func (c *ModelClient) Consistency(ctx context.Context, pairs []ModelPair) ([]ModelResult, int, error) {
	if len(pairs) == 0 {
		return nil, 0, nil
	}
	if len(pairs) > modelMaxPairs {
		pairs = pairs[:modelMaxPairs]
	}
	body, err := json.Marshal(map[string]any{"pairs": pairs})
	if err != nil {
		return nil, 0, fmt.Errorf("encode: %w", err)
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/v1/consistency", bytes.NewReader(body),
	)
	if err != nil {
		return nil, 0, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("call sidecar: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("sidecar status %d", res.StatusCode)
	}
	var decoded struct {
		Results []ModelResult `json:"results"`
		Pending int           `json:"pending"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return nil, 0, fmt.Errorf("decode: %w", err)
	}
	return decoded.Results, decoded.Pending, nil
}

package ai

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Path parameter names for the tools routes.
const (
	MuteRefParam    = "mute"
	LexiconRefParam = "term"
)

// ToolsHandler exposes the 13Y endpoints. Parsing and shaping only; every
// decision is the Tools service's.
type ToolsHandler struct {
	tools *Tools
}

func NewToolsHandler(tools *Tools) *ToolsHandler { return &ToolsHandler{tools: tools} }

// Check handles POST /api/v1/ai/check - the editor's live pass.
func (h *ToolsHandler) Check(c *gin.Context) {
	var body struct {
		Novel string `json:"novel"`
		Mode  string `json:"mode"`
		Text  string `json:"text"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	result, err := h.tools.Check(c.Request.Context(), identityOf(c), CheckInput{
		NovelRef: body.Novel, Mode: body.Mode, Text: body.Text,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// ConvertChat handles POST /api/v1/ai/convert-chat - the fiction-format
// conversion engine (docs/CHAT-CONVERSION.md). Read-only: it returns the
// structure; writing messages stays the author's explicit save.
func (h *ToolsHandler) ConvertChat(c *gin.Context) {
	var body struct {
		Novel string `json:"novel"`
		Text  string `json:"text"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	result, err := h.tools.ConvertChat(c.Request.Context(), identityOf(c), body.Novel, body.Text)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// CharacterCheck handles POST /api/v1/ai/character-check.
func (h *ToolsHandler) CharacterCheck(c *gin.Context) {
	var body struct {
		Novel         string `json:"novel"`
		ChapterNumber int    `json:"chapter_number"`
		Text          string `json:"text"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	result, err := h.tools.CharacterCheck(c.Request.Context(), identityOf(c),
		body.Novel, body.ChapterNumber, body.Text)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// SetEvolution handles PUT /api/v1/ai/character-evolution - the "ตัวละคร
// เปลี่ยนไปตั้งแต่ตอนนี้" marker. from_chapter_number 0 clears it.
func (h *ToolsHandler) SetEvolution(c *gin.Context) {
	var body struct {
		Novel             string `json:"novel"`
		CharacterID       string `json:"character_id"`
		FromChapterNumber int    `json:"from_chapter_number"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	characterID, err := uuid.Parse(body.CharacterID)
	if err != nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"character_id": {"A valid character id is required."},
		}))
		return
	}
	if err := h.tools.SetEvolution(c.Request.Context(), identityOf(c),
		body.Novel, characterID, body.FromChapterNumber); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Continuity handles POST /api/v1/ai/continuity.
func (h *ToolsHandler) Continuity(c *gin.Context) {
	var body struct {
		Novel     string `json:"novel"`
		ChapterID string `json:"chapter_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	chapterID, err := uuid.Parse(body.ChapterID)
	if err != nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"chapter_id": {"A valid chapter id is required."},
		}))
		return
	}
	result, serviceErr := h.tools.ContinuityCheck(c.Request.Context(), identityOf(c),
		body.Novel, chapterID)
	if serviceErr != nil {
		response.Fail(c, serviceErr)
		return
	}
	response.OK(c, result)
}

// Precheck handles POST /api/v1/ai/precheck - the pre-publish round.
func (h *ToolsHandler) Precheck(c *gin.Context) {
	var body struct {
		Novel     string `json:"novel"`
		ChapterID string `json:"chapter_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	chapterID, err := uuid.Parse(body.ChapterID)
	if err != nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"chapter_id": {"A valid chapter id is required."},
		}))
		return
	}
	result, serviceErr := h.tools.Precheck(c.Request.Context(), identityOf(c),
		body.Novel, chapterID)
	if serviceErr != nil {
		response.Fail(c, serviceErr)
		return
	}
	response.OK(c, result)
}

// GetPrefs handles GET /api/v1/ai/prefs?novel=… .
func (h *ToolsHandler) GetPrefs(c *gin.Context) {
	view, err := h.tools.GetPrefs(c.Request.Context(), identityOf(c), c.Query("novel"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// SetPrefs handles PUT /api/v1/ai/prefs - one tier per call.
func (h *ToolsHandler) SetPrefs(c *gin.Context) {
	var body struct {
		Novel string `json:"novel"`
		Prefs Prefs  `json:"prefs"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	view, err := h.tools.SetPrefs(c.Request.Context(), identityOf(c), body.Novel, body.Prefs)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Lexicon handles GET /api/v1/novels/:novel/lexicon.
func (h *ToolsHandler) Lexicon(c *gin.Context) {
	view, err := h.tools.Lexicon(c.Request.Context(), identityOf(c), c.Param("novel"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// AddLexiconTerm handles POST /api/v1/novels/:novel/lexicon.
func (h *ToolsHandler) AddLexiconTerm(c *gin.Context) {
	var body struct {
		Term string `json:"term"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	view, err := h.tools.AddLexiconTerm(c.Request.Context(), identityOf(c),
		c.Param("novel"), body.Term)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// RemoveLexiconTerm handles DELETE /api/v1/novels/:novel/lexicon/:term.
func (h *ToolsHandler) RemoveLexiconTerm(c *gin.Context) {
	termID, err := uuid.Parse(c.Param(LexiconRefParam))
	if err != nil {
		response.Fail(c, apierror.New(404, "TERM_NOT_FOUND", "Term not found."))
		return
	}
	if err := h.tools.RemoveLexiconTerm(c.Request.Context(), identityOf(c),
		c.Param("novel"), termID); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// UserLexicon handles GET /api/v1/ai/lexicon - the account-wide word bank.
func (h *ToolsHandler) UserLexicon(c *gin.Context) {
	view, err := h.tools.UserLexicon(c.Request.Context(), identityOf(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// AddUserLexiconTerm handles POST /api/v1/ai/lexicon.
func (h *ToolsHandler) AddUserLexiconTerm(c *gin.Context) {
	var body struct {
		Term string `json:"term"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	view, err := h.tools.AddUserLexiconTerm(c.Request.Context(), identityOf(c), body.Term)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// RemoveUserLexiconTerm handles DELETE /api/v1/ai/lexicon/:term.
func (h *ToolsHandler) RemoveUserLexiconTerm(c *gin.Context) {
	termID, err := uuid.Parse(c.Param(LexiconRefParam))
	if err != nil {
		response.Fail(c, apierror.New(404, "TERM_NOT_FOUND", "Term not found."))
		return
	}
	if err := h.tools.RemoveUserLexiconTerm(c.Request.Context(), identityOf(c), termID); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// AddMute handles POST /api/v1/ai/mutes - "ไม่เตือนแบบนี้อีก".
func (h *ToolsHandler) AddMute(c *gin.Context) {
	var body struct {
		Novel string `json:"novel"`
		Kind  string `json:"kind"`
		Term  string `json:"term"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	if err := h.tools.AddMute(c.Request.Context(), identityOf(c),
		body.Novel, body.Kind, body.Term); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// ListMutes handles GET /api/v1/ai/mutes?novel=… .
func (h *ToolsHandler) ListMutes(c *gin.Context) {
	mutes, err := h.tools.ListMutes(c.Request.Context(), identityOf(c), c.Query("novel"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"mutes": mutes})
}

// RemoveMute handles DELETE /api/v1/ai/mutes/:mute.
func (h *ToolsHandler) RemoveMute(c *gin.Context) {
	muteID, err := uuid.Parse(c.Param(MuteRefParam))
	if err != nil {
		response.Fail(c, apierror.New(404, "MUTE_NOT_FOUND", "Mute not found."))
		return
	}
	if err := h.tools.RemoveMute(c.Request.Context(), identityOf(c), muteID); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Facts handles GET /api/v1/novels/:novel/chapters/:chapter/facts.
func (h *ToolsHandler) Facts(c *gin.Context) {
	chapterID, err := uuid.Parse(c.Param("chapter"))
	if err != nil {
		response.Fail(c, apierror.New(404, "CHAPTER_NOT_FOUND", "Chapter not found."))
		return
	}
	facts, serviceErr := h.tools.Facts(c.Request.Context(), identityOf(c),
		c.Param("novel"), chapterID)
	if serviceErr != nil {
		response.Fail(c, serviceErr)
		return
	}
	response.OK(c, gin.H{"facts": facts})
}

// SetFacts handles PUT /api/v1/novels/:novel/chapters/:chapter/facts.
func (h *ToolsHandler) SetFacts(c *gin.Context) {
	chapterID, err := uuid.Parse(c.Param("chapter"))
	if err != nil {
		response.Fail(c, apierror.New(404, "CHAPTER_NOT_FOUND", "Chapter not found."))
		return
	}
	var body struct {
		Facts []Fact `json:"facts"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	facts, serviceErr := h.tools.SetFacts(c.Request.Context(), identityOf(c),
		c.Param("novel"), chapterID, body.Facts)
	if serviceErr != nil {
		response.Fail(c, serviceErr)
		return
	}
	response.OK(c, gin.H{"facts": facts})
}

// Search handles GET /api/v1/novels/:novel/search?q=… .
func (h *ToolsHandler) Search(c *gin.Context) {
	hits, err := h.tools.Search(c.Request.Context(), identityOf(c),
		c.Param("novel"), c.Query("q"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"results": hits})
}

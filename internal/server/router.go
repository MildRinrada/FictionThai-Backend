package server

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/achievements"
	"github.com/fictionthai/fictionthai/backend/internal/ai"
	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/authors"
	"github.com/fictionthai/fictionthai/backend/internal/chapters"
	"github.com/fictionthai/fictionthai/backend/internal/characters"
	"github.com/fictionthai/fictionthai/backend/internal/comments"
	"github.com/fictionthai/fictionthai/backend/internal/community"
	"github.com/fictionthai/fictionthai/backend/internal/config"
	"github.com/fictionthai/fictionthai/backend/internal/desk"
	"github.com/fictionthai/fictionthai/backend/internal/fiction"
	"github.com/fictionthai/fictionthai/backend/internal/health"
	"github.com/fictionthai/fictionthai/backend/internal/insights"
	"github.com/fictionthai/fictionthai/backend/internal/library"
	"github.com/fictionthai/fictionthai/backend/internal/media"
	"github.com/fictionthai/fictionthai/backend/internal/middleware"
	"github.com/fictionthai/fictionthai/backend/internal/moderation"
	"github.com/fictionthai/fictionthai/backend/internal/notifications"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/pennames"
	"github.com/fictionthai/fictionthai/backend/internal/platform/cache"
	"github.com/fictionthai/fictionthai/backend/internal/platform/database"
	"github.com/fictionthai/fictionthai/backend/internal/profiles"
	"github.com/fictionthai/fictionthai/backend/internal/promo"
	"github.com/fictionthai/fictionthai/backend/internal/ratelimit"
	"github.com/fictionthai/fictionthai/backend/internal/shelves"
	"github.com/fictionthai/fictionthai/backend/internal/subscriptions"
	"github.com/fictionthai/fictionthai/backend/internal/taxonomy"
	"github.com/fictionthai/fictionthai/backend/internal/variables"
	"github.com/fictionthai/fictionthai/backend/internal/views"
	"github.com/fictionthai/fictionthai/backend/internal/wall"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// APIVersion is the URL prefix for the current API version. Breaking changes
// introduce /api/v2 rather than mutating this one (docs/09 §42).
const APIVersion = "v1"

// apiBasePath is the mount point for every versioned resource.
const apiBasePath = "/api/" + APIVersion

// Dependencies are the collaborators a router needs. Passing them explicitly
// keeps the router testable without global state.
type Dependencies struct {
	Config  *config.Config
	Logger  *slog.Logger
	DB      *database.DB
	Cache   *cache.Client
	Limiter ratelimit.Limiter
	Version string

	// Auth is nil in tests that exercise only public routes; the authentication
	// middleware and its endpoints are then omitted.
	Auth *auth.Service

	// Novels and Chapters are nil in tests that exercise only the operational
	// or authentication routes; the publishing endpoints are then omitted.
	Novels   *novels.Service
	Chapters *chapters.Service

	// Insights is nil in tests that exercise only earlier phases; the studio
	// overview endpoint is then omitted (§13R).
	Insights *insights.Service

	// Desk is nil in tests that exercise only earlier phases; the writer's
	// shell read is then omitted and the header simply draws no badge.
	Desk *desk.Service

	// Library is nil in tests that exercise only earlier phases; the shelf
	// endpoints are then omitted.
	Library *library.Service

	// Taxonomy is nil in tests that exercise only earlier phases; the
	// discovery endpoints are then omitted.
	Taxonomy *taxonomy.Service

	// Comments and Notifications are nil in tests that exercise only earlier
	// phases; the interaction endpoints are then omitted.
	Comments      *comments.Service
	Notifications *notifications.Service

	// Community is nil in tests that exercise only earlier phases; the
	// community endpoints are then omitted.
	Community *community.Service

	// Moderation is nil in tests that exercise only earlier phases; the
	// report and moderation endpoints are then omitted.
	Moderation *moderation.Service

	// Media is nil in tests that exercise only earlier phases; the upload,
	// delete, and file-serving endpoints are then omitted.
	Media *media.Service

	// AI is nil in tests that exercise only earlier phases; the AI / Thai NLP
	// endpoints are then omitted.
	AI *ai.Service

	// AITools is the 13Y writing-tools service (word bank, character check,
	// fact book, search, prefs). nil omits those routes.
	AITools *ai.Tools

	// Subscription and Authors are nil in tests that exercise only earlier
	// phases; the Phase 11 Premium and author-profile endpoints are then omitted.
	Subscription *subscriptions.Service
	Authors      *authors.Service

	// Characters is nil in tests that exercise only earlier phases; the Phase
	// 12A cast endpoints are then omitted.
	Characters *characters.Service

	// Variables is nil in tests that exercise only earlier phases; the Phase
	// 13H reader-variable endpoints are then omitted.
	Variables *variables.Service

	// Promo is the home hero's slide queue (docs/HOME-PROMO.md). Nil skips the
	// routes, like every optional domain.
	Promo *promo.Service

	// Profiles is nil in tests that exercise only earlier phases; the Phase 12E
	// public profile read is then omitted.
	Profiles *profiles.Service

	// Shelves and Wall are nil in tests that exercise only earlier phases; the
	// public-bookshelf and profile-wall endpoints are then omitted.
	Shelves *shelves.Service
	Wall    *wall.Service

	// PenNames is nil in tests that exercise only earlier phases; the
	// self-scoped pen-name endpoints are then omitted
	// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2).
	PenNames *pennames.Service

	// Achievements is nil in tests that exercise only earlier phases; the
	// achievement endpoints are then omitted, and the domains that signal into
	// it simply signal nowhere
	// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3).
	Achievements *achievements.Service

	// Views counts reads (Phase 12C). Nil - or present but disabled, when Redis
	// is unavailable - simply counts nothing; it can never fail a read.
	Views *views.Recorder
}

// NewRouter wires middleware and routes.
//
// Middleware order is deliberate:
//
//	RequestID    first, so every later log line and response carries the ID
//	Recovery     early, so it catches panics from everything after it
//	Logger       after recovery, so it still records the 500 it produced
//	Security     before handlers, so headers are set even on early aborts
//	CORS         before anything that can reject, so a preflight always answers
//	BodyLimit    before handlers read the body
//	Authenticate last of the global chain - it may query the database, so
//	             everything cheap runs first, and it must run BEFORE the rate
//	             limiter so an authenticated request is keyed by user ID
//
// Per-route middleware then composes: RateLimit → CSRF → RequireAuth → handler.
func NewRouter(deps Dependencies) *gin.Engine {
	if deps.Config.App.IsProduction() || deps.Config.App.Env == config.EnvStaging {
		gin.SetMode(gin.ReleaseMode)
	} else if deps.Config.App.Env == config.EnvTest {
		gin.SetMode(gin.TestMode)
	}

	// gin.New, not gin.Default: Default installs gin's own unstructured logger,
	// which would duplicate our structured one.
	r := gin.New()

	// Only trust forwarding headers from the reverse proxy in front of us
	// (docs/14 §10). Without this, X-Forwarded-For could be spoofed to evade
	// the IP-keyed rate limiter.
	if err := r.SetTrustedProxies(nil); err != nil {
		deps.Logger.Warn("could not configure trusted proxies", slog.Any("error", err))
	}

	r.HandleMethodNotAllowed = true
	r.NoRoute(middleware.NoRoute())
	r.NoMethod(middleware.NoMethod())

	r.Use(
		middleware.RequestID(),
		middleware.Recovery(deps.Logger),
		middleware.Logger(deps.Logger),
		middleware.SecurityHeaders(deps.Config.App),
		middleware.CORS(deps.Config.CORS),
		middleware.BodyLimit(deps.Config.HTTP.MaxRequestBytes),
	)

	cookies := auth.CookieConfig{Secure: deps.Config.Session.SecureCookies}

	if deps.Auth != nil {
		// Optional authentication: a guest passes through with a nil identity.
		r.Use(middleware.Authenticate(deps.Auth, cookies, deps.Logger))
	}

	registerOperationalRoutes(r, deps)
	registerAPIRoutes(r, deps, cookies)

	return r
}

// registerOperationalRoutes mounts probes and the service root. These live
// OUTSIDE /api/v1: they are operational endpoints for infrastructure, not
// versioned product resources, so they intentionally do not use the
// {"data": ...} envelope.
func registerOperationalRoutes(r *gin.Engine, deps Dependencies) {
	probes := health.NewHandler(
		deps.Version,
		health.PostgresChecker{DB: deps.DB},
		health.RedisChecker{Client: deps.Cache},
	)

	r.GET("/health", probes.Live)
	r.GET("/ready", probes.Ready)

	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"name":        deps.Config.App.Name,
			"version":     deps.Version,
			"api_version": APIVersion,
			"api_base":    apiBasePath,
			"status":      health.StatusOK,
			"docs":        "See /docs in the repository for the API specification.",
		})
	})
}

// registerAPIRoutes mounts the versioned API.
func registerAPIRoutes(r *gin.Engine, deps Dependencies, cookies auth.CookieConfig) {
	v1 := r.Group(apiBasePath)

	csrf := middleware.CSRF(cookies, deps.Config.CORS.AllowedOrigins)

	// --- Public reads ------------------------------------------------------
	// The generous public tier: content protection must never interfere with
	// normal reading (docs/07 §32). No authentication of any kind is required
	// here - this is the guest-first path (docs/11 §12).
	public := v1.Group("")
	public.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		// Publishes the Fiction Format System vocabulary so web and mobile
		// clients derive badges, filters, and reader selection from the server
		// rather than hard-coding their own copies (docs/09 §51).
		public.GET("/fiction-formats", func(c *gin.Context) {
			response.OK(c, gin.H{
				"story_structures":     fiction.StoryStructures(),
				"presentation_formats": fiction.PresentationFormats(),
				"content_modes":        fiction.ContentModes(),
				"defaults":             fiction.DefaultFormat(),
				"novel_statuses":       novels.Statuses(),
				"visibilities":         novels.Visibilities(),
				"chapter_statuses":     chapters.Statuses(),
				"message_types":        chapters.MessageTypes(),
				"sort_options":         novels.SortOptions(),
			})
		})
	}

	registerPublishingRoutes(v1, deps, csrf)
	registerLibraryRoutes(v1, deps, csrf)
	registerDiscoveryRoutes(v1, deps, csrf)
	registerInteractionRoutes(v1, deps, csrf)
	registerCommunityRoutes(v1, deps, csrf)
	registerModerationRoutes(v1, deps, csrf)
	registerMediaRoutes(r, v1, deps, csrf)
	registerAIRoutes(v1, deps, csrf)
	registerSubscriptionRoutes(v1, deps, csrf)
	registerAuthorRoutes(v1, deps, csrf)
	registerCharacterRoutes(v1, deps, csrf)
	registerVariableRoutes(v1, deps, csrf)
	registerProfileRoutes(v1, deps, csrf)
	registerPenNameRoutes(v1, deps, csrf)
	registerAchievementRoutes(v1, deps, csrf)
	registerShelfRoutes(v1, deps, csrf)
	registerDeskRoutes(v1, deps)
	registerWallRoutes(v1, deps, csrf)
	registerPromoRoutes(v1, deps, csrf)

	if deps.Auth == nil {
		return
	}

	handler := auth.NewHandler(deps.Auth, cookies)

	// --- Unauthenticated auth endpoints ------------------------------------
	// Strict tier: these are the brute-force, credential-stuffing, and
	// enumeration targets (docs/10 §38, docs/11 §25).
	//
	// No CSRF here. There is no session to ride on yet, so there is nothing for
	// a cross-site request to abuse - and requiring a token would make the
	// login form unusable for a first-time visitor.
	authPublic := v1.Group("/auth")
	authPublic.Use(middleware.RateLimit(deps.Limiter, ratelimit.Auth))
	{
		authPublic.POST("/register", handler.Register)
		authPublic.POST("/login", handler.Login)
		authPublic.POST("/password/forgot", handler.ForgotPassword)
		authPublic.POST("/password/reset", handler.ResetPassword)
		authPublic.POST("/verify-email", handler.VerifyEmail)
	}

	// --- Authenticated auth endpoints --------------------------------------
	// CSRF applies: these mutate session state and can be reached with an
	// ambient cookie. Logout is a real CSRF target - forcing a victim to log
	// out is a denial-of-service, and forcing logout-all is worse.
	authPrivate := v1.Group("/auth")
	authPrivate.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
	)
	{
		authPrivate.POST("/logout", handler.Logout)
		authPrivate.POST("/logout-all", handler.LogoutAll)

		// The author's one-time adult statement (§13B). A state change on the
		// account, so it belongs here rather than beside the profile fields.
		authPrivate.POST("/adult-attestation", handler.AttestAdult)
	}

	// GET /auth/me is a read, so it needs no CSRF, but it does require a
	// signed-in caller - docs/09 §12 specifies 401 for a guest.
	me := v1.Group("/auth")
	me.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead), middleware.RequireAuth())
	{
		me.GET("/me", handler.Me)
	}
}

// registerPublishingRoutes mounts the fiction and chapter endpoints
// (docs/09 §15, §16, §48).
//
// The path parameter is named once, in novels.RefParam, and reused for the whole
// subtree: Gin allows only one wildcard name per path position, and this is what
// reconciles docs/09 reading by slug with writing by id. Both forms resolve to
// the same row and are authorized identically.
//
// Reads and writes are separated deliberately:
//
//	reads   guest-first, generous tier, NO authentication of any kind
//	writes  strict tier, CSRF for cookie callers, RequireAuth
//
// Ownership is NOT enforced here. Middleware sees a route; only the service can
// see who owns the row behind it, so every write handler re-checks (docs/10 §27).
func registerPublishingRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Novels == nil || deps.Chapters == nil {
		return
	}

	novelHandler := novels.NewHandler(deps.Novels)
	chapterHandler := chapters.NewHandler(deps.Chapters)
	if deps.Views != nil {
		chapterHandler = chapterHandler.WithViewCounter(deps.Views)
	}

	novelPath := "/novels/:" + novels.RefParam
	chapterPath := novelPath + "/chapters/:" + chapters.RefParam

	// --- Public reads ------------------------------------------------------
	// Guests browse and read without an account (docs/09 §6, docs/11 §12). No
	// session lookup is forced: the Authenticate middleware is optional and a
	// guest simply passes through with a nil identity, so the reader path costs
	// no authentication query (Phase 2 brief §21).
	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		reads.GET("/novels", novelHandler.List)
		reads.GET(novelPath, novelHandler.Get)
		reads.GET(novelPath+"/chapters", chapterHandler.List)
		reads.GET(chapterPath, chapterHandler.Get)
	}

	if deps.Auth == nil {
		return
	}

	// --- Authenticated writes ----------------------------------------------
	writes := v1.Group("")
	writes.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
	)
	{
		writes.POST("/novels", novelHandler.Create)
		writes.PATCH(novelPath, novelHandler.Update)
		writes.PATCH(novelPath+"/format", novelHandler.UpdateFormat)
		// The pre-publish checklist (§13L). A read, but owner-only, so it sits
		// with the writes rather than on the public-read group.
		writes.GET(novelPath+"/readiness", novelHandler.Readiness)

		// The studio overview's numbers and activity feed (§13R). Owner-only for
		// the same reason, and mounted here so it inherits the same session and
		// CSRF treatment as everything else a writer's own studio calls.
		if deps.Insights != nil {
			writes.GET(novelPath+"/insights", insights.NewHandler(deps.Insights).Get)
		}
		writes.DELETE(novelPath, novelHandler.Delete)

		// ผู้เขียนร่วม (13U). Owner-managed; the list itself is public credit
		// and rides on the fiction view, so GET here is for the settings page.
		writes.GET(novelPath+"/collaborators", novelHandler.ListCollaborators)
		writes.POST(novelPath+"/collaborators", novelHandler.AddCollaborator)
		writes.DELETE(novelPath+"/collaborators/:collaborator", novelHandler.RemoveCollaborator)

		writes.POST(novelPath+"/chapters", chapterHandler.Create)
		writes.PATCH(chapterPath, chapterHandler.Update)
		writes.DELETE(chapterPath, chapterHandler.Delete)

		// Revision history (docs/01 §16): the snapshots every save records,
		// finally reachable. GET sits in the writes group because history is
		// editor-only - the service gate is ForEditor either way.
		writes.GET(chapterPath+"/revisions", chapterHandler.Revisions)
		writes.POST(chapterPath+"/revisions/:version/restore", chapterHandler.RestoreRevision)
		// Reordering renumbers to 1..N - content arrangement, not publication,
		// so it lives with the ordinary writes (13X).
		writes.PUT(novelPath+"/chapters/order", chapterHandler.Reorder)

		// Unpublishing RETRACTS content, so it is deliberately not behind the
		// verification gate below. Making it harder to take work down than to
		// put it up would be the wrong way round.
		writes.POST(chapterPath+"/unpublish", chapterHandler.Unpublish)
	}

	// --- Publishing ---------------------------------------------------------
	// The one place RequireVerifiedEmail applies. docs/10 §17 and
	// docs/AUTHENTICATION.md §9: verification gates PUBLISHING, never reading or
	// ordinary account use - so it is absent from every route above.
	//
	// The services enforce the same rule for the other paths that expose work
	// (creating a published chapter, making a fiction public), because those
	// arrive through routes that also serve legitimate draft edits.
	publishing := v1.Group("")
	publishing.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
		middleware.RequireVerifiedEmail(),
	)
	{
		publishing.POST(chapterPath+"/publish", chapterHandler.Publish)
	}
}

// registerDiscoveryRoutes mounts the Phase 4 discovery endpoints
// (docs/09 §11, §22; docs/08 §14–§15).
//
//	vocabulary reads  guest-first, generous tier - browsing needs no account
//	search            its own Search tier (docs/09 §31): costlier than a
//	                  keyed read, cheaper limit than auth endpoints
//	tag creation      Write tier + CSRF + RequireAuth - the one authenticated
//	                  operation here; genres have no API write path at all
//	                  (curated vocabulary, changed operationally)
func registerDiscoveryRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Taxonomy == nil {
		return
	}

	handler := taxonomy.NewHandler(deps.Taxonomy)

	// --- Public vocabulary reads -------------------------------------------
	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		reads.GET("/genres", handler.Genres)
		reads.GET("/tags", handler.Tags)
	}

	// --- Search ------------------------------------------------------------
	// Search is its own concern (docs/09 §22) and its own rate class
	// (docs/09 §31) - the handler lives in novels because search RESULTS are
	// fictions, filtered by the same guest-first visibility as every listing.
	if deps.Novels != nil {
		search := v1.Group("")
		search.Use(middleware.RateLimit(deps.Limiter, ratelimit.Search))
		{
			search.GET("/search/novels", novels.NewHandler(deps.Novels).Search)
			// The filter panel's per-option counts (search review 2026-08
			// section A), under the same parameters the search takes.
			search.GET("/search/facets", novels.NewHandler(deps.Novels).SearchFacets)
			// นักเขียน - the people half of search. Public, like every other
			// kind of discovery here: a reader who just finished a story must
			// be able to find its author without an account first.
			if deps.Profiles != nil {
				search.GET("/search/authors", profiles.NewHandler(deps.Profiles).SearchAuthors)
			}
			// ชั้นหนังสือสาธารณะ - opt-in public shelves, findable by name
			// (search review section F).
			if deps.Shelves != nil {
				search.GET("/search/shelves", shelves.NewHandler(deps.Shelves).SearchPublic)
			}
		}
	}

	// --- Tag creation ------------------------------------------------------
	if deps.Auth != nil {
		writes := v1.Group("")
		writes.Use(
			middleware.RateLimit(deps.Limiter, ratelimit.Write),
			csrf,
			middleware.RequireAuth(),
		)
		{
			writes.POST("/tags", handler.CreateTag)
		}
	}
}

// registerInteractionRoutes mounts the Phase 6 endpoints: comments and
// notifications (docs/09 §20, §23; docs/08 §20, §23).
//
//	comment reads     guest-first, generous tier - reading the discussion is
//	                  part of reading the fiction (docs/03 §27: guests read,
//	                  members comment)
//	comment writes    Write tier + CSRF + RequireAuth. docs/11 §24 lists
//	                  comment creation as a rate-limited class; it shares the
//	                  Write tier rather than growing a new one. NO email
//	                  verification: that gates publishing fiction only
//	                  (docs/AUTHENTICATION.md §9).
//	notifications     all RequireAuth - there is no public surface at all.
//	                  Reads are cheap keyed reads on the PublicRead tier like
//	                  the other /me reads; the two mutations take the Write
//	                  tier + CSRF.
func registerInteractionRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Comments != nil {
		handler := comments.NewHandler(deps.Comments)

		novelPath := "/novels/:" + novels.RefParam
		chapterPath := novelPath + "/chapters/:" + chapters.RefParam
		commentPath := "/comments/:" + comments.RefParam

		reads := v1.Group("")
		reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
		{
			reads.GET(novelPath+"/comments", handler.ListForNovel)
			reads.GET(chapterPath+"/comments", handler.ListForChapter)
			reads.GET(commentPath+"/replies", handler.ListReplies)
		}

		if deps.Auth != nil {
			// Posting is NOT behind RequireAuth since §13D: a fiction whose
			// author chose "ทุกคน" accepts a comment from a reader with no
			// account, and the SERVICE decides - it has the fiction in hand and
			// can see which of the three levels applies, which middleware
			// cannot (docs/10 §27). A guest on a members-only fiction still
			// gets the 401 they would have got here.
			//
			// CSRF still applies. A guest carries no session for a cross-site
			// request to ride, so the token is a no-op for them; a signed-in
			// reader posting through the same route is protected exactly as
			// before, and one route with one rule beats two routes that could
			// drift apart.
			posts := v1.Group("")
			posts.Use(middleware.RateLimit(deps.Limiter, ratelimit.Write), csrf)
			{
				posts.POST(novelPath+"/comments", handler.CreateForNovel)
				posts.POST(chapterPath+"/comments", handler.CreateForChapter)
				posts.POST(commentPath+"/replies", handler.Reply)
			}

			writes := v1.Group("")
			writes.Use(
				middleware.RateLimit(deps.Limiter, ratelimit.Write),
				csrf,
				middleware.RequireAuth(),
			)
			{
				writes.PATCH(commentPath, handler.Update)
				writes.DELETE(commentPath, handler.Delete)

				// ถูกใจ (comment design review 2026-08). Members only, and
				// idempotent both ways (docs/09 §33's shape for likes).
				writes.POST(commentPath+"/like", handler.Like)
				writes.DELETE(commentPath+"/like", handler.Unlike)

				// ตรวจก่อนโพสต์ (§13D). Owner-only, decided in the service
				// through the fiction's own ownership rule; the queue read is a
				// write-tier route because it is a studio surface, not a
				// reader one.
				writes.POST(commentPath+"/approve", handler.Approve)
				writes.POST(commentPath+"/reject", handler.Reject)
			}

			queue := v1.Group("")
			queue.Use(
				middleware.RateLimit(deps.Limiter, ratelimit.PublicRead),
				middleware.RequireAuth(),
			)
			{
				queue.GET(novelPath+"/comments/pending", handler.ListPending)
			}
		}
	}

	if deps.Notifications != nil && deps.Auth != nil {
		handler := notifications.NewHandler(deps.Notifications)

		reads := v1.Group("")
		reads.Use(
			middleware.RateLimit(deps.Limiter, ratelimit.PublicRead),
			middleware.RequireAuth(),
		)
		{
			reads.GET("/me/notifications", handler.List)
			reads.GET("/me/notifications/unread-count", handler.UnreadCount)
		}

		writes := v1.Group("")
		writes.Use(
			middleware.RateLimit(deps.Limiter, ratelimit.Write),
			csrf,
			middleware.RequireAuth(),
		)
		{
			writes.POST("/notifications/:"+notifications.RefParam+"/read", handler.MarkRead)
			writes.POST("/me/notifications/read-all", handler.MarkAllRead)
		}
	}
}

// registerCommunityRoutes mounts the Phase 7 endpoints: community posts,
// their comment threads, and reactions (docs/09 §21; docs/08 §21).
//
//	reads      guest-first, generous tier - docs/03 §27: guests read public
//	           community content. Visibility (public/followers/private) is
//	           enforced INSIDE the service (docs/11 §37), which sees the
//	           optional identity; the route stays unauthenticated.
//	writes     Write tier + CSRF + RequireAuth (docs/11 §24 lists community
//	           posting as a rate-limited class; it shares the Write tier).
//	           NO email verification: any signed-in user may post
//	           (docs/03 §27 access matrix); verification gates publishing
//	           FICTION only.
func registerCommunityRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Community == nil {
		return
	}

	handler := community.NewHandler(deps.Community)

	postPath := "/community/posts/:" + community.PostRefParam
	commentPath := "/community/comments/:" + community.CommentRefParam

	// --- Public reads ------------------------------------------------------
	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		reads.GET("/community/posts", handler.ListPosts)
		// Static segment, registered beside /community/posts rather than under
		// it, so no id can ever shadow it.
		reads.GET("/community/discussed", handler.ListDiscussedFictions)
		reads.GET("/community/tags", handler.ListTrendingTags)
		reads.GET(postPath, handler.GetPost)
		reads.GET(postPath+"/comments", handler.ListComments)
		reads.GET(commentPath+"/replies", handler.ListReplies)
	}

	// --- Post search -------------------------------------------------------
	// Beside the other /search/* endpoints and on the same tier
	// (docs/COMMUNITY-FEED.md): an ILIKE scan is more expensive than a keyed
	// read, so it must not ride the generous PublicRead class. Guest-first
	// like every search; the service enforces its public-only rule.
	search := v1.Group("")
	search.Use(middleware.RateLimit(deps.Limiter, ratelimit.Search))
	{
		search.GET("/search/posts", handler.SearchPosts)
	}

	// --- Authenticated writes ----------------------------------------------
	if deps.Auth != nil {
		writes := v1.Group("")
		writes.Use(
			middleware.RateLimit(deps.Limiter, ratelimit.Write),
			csrf,
			middleware.RequireAuth(),
		)
		{
			writes.POST("/community/posts", handler.CreatePost)
			writes.PATCH(postPath, handler.UpdatePost)
			writes.DELETE(postPath, handler.DeletePost)

			writes.POST(postPath+"/comments", handler.CreateComment)
			writes.POST(commentPath+"/replies", handler.Reply)
			writes.PATCH(commentPath, handler.UpdateComment)
			writes.DELETE(commentPath, handler.DeleteComment)

			writes.POST(postPath+"/reactions", handler.React)
			writes.DELETE(postPath+"/reactions", handler.RemoveReaction)

			writes.POST(postPath+"/bookmark", handler.Bookmark)
			writes.DELETE(postPath+"/bookmark", handler.RemoveBookmark)
		}
	}
}

// registerPromoRoutes mounts the home hero's slide queue (docs/HOME-PROMO.md).
//
//	GET  /promo/slides               public: the live deck, rules applied.
//	POST /promo/slides/:slide/click  public counter; anonymous by design -
//	                                 guests click slides too, and the number
//	                                 is an indicator, never billing.
//	/admin/promo/*                   RequireAuth + RequireStaff, and the
//	                                 service re-checks - the moderation
//	                                 double gate (docs/10 §48).
func registerPromoRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Promo == nil {
		return
	}

	handler := promo.NewHandler(deps.Promo)
	slidePath := "/admin/promo/slides/:" + promo.SlideRefParam

	public := v1.Group("")
	public.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		public.GET("/promo/slides", handler.Active)
		public.POST("/promo/slides/:"+promo.SlideRefParam+"/click", handler.Click)
	}

	if deps.Auth == nil {
		return
	}
	staffReads := v1.Group("")
	staffReads.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.PublicRead),
		middleware.RequireAuth(),
		middleware.RequireStaff(),
	)
	{
		staffReads.GET("/admin/promo/slides", handler.Queue)
	}

	staffWrites := v1.Group("")
	staffWrites.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
		middleware.RequireStaff(),
	)
	{
		staffWrites.POST("/admin/promo/slides", handler.Create)
		staffWrites.PUT("/admin/promo/slides/order", handler.Reorder)
		staffWrites.PATCH(slidePath, handler.Update)
		staffWrites.DELETE(slidePath, handler.Delete)
	}
}

// registerModerationRoutes mounts the Phase 8 endpoints: user reports and the
// staff moderation surface (docs/09 §28–§29; docs/08 §24).
//
//	POST /reports       Write tier + CSRF + RequireAuth. Reporting is
//	                    user-generated input; docs/11 §24 documents no
//	                    dedicated report class, so it shares the Write tier
//	                    with comments and posts. Guests cannot report - the
//	                    reporter identity is a required report field
//	                    (docs/01 §21) - but nothing here touches the public
//	                    READING paths, which stay guest-first.
//	GET  /me/reports    the caller's own filing history: cheap keyed read,
//	                    PublicRead tier + RequireAuth like the other /me reads.
//	/admin/*            RequireAuth + RequireStaff on every route
//	                    (docs/09 §29 "isolated by authorization",
//	                    docs/03 §27). Role gets a caller through the DOOR;
//	                    the service re-checks staff-ness and every target
//	                    still enforces its own rules (docs/10 §27, §48).
func registerModerationRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Moderation == nil || deps.Auth == nil {
		return
	}

	handler := moderation.NewHandler(deps.Moderation)
	reportPath := "/admin/reports/:" + moderation.RefParam

	// --- User side ---------------------------------------------------------
	userReads := v1.Group("")
	userReads.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.PublicRead),
		middleware.RequireAuth(),
	)
	{
		userReads.GET("/me/reports", handler.MyReports)
	}

	userWrites := v1.Group("")
	userWrites.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
	)
	{
		userWrites.POST("/reports", handler.CreateReport)
	}

	// --- Staff side --------------------------------------------------------
	staffReads := v1.Group("")
	staffReads.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.PublicRead),
		middleware.RequireAuth(),
		middleware.RequireStaff(),
	)
	{
		staffReads.GET("/admin/reports", handler.Queue)
		staffReads.GET(reportPath, handler.GetReport)
		staffReads.GET("/admin/moderation/actions", handler.ListActions)
	}

	staffWrites := v1.Group("")
	staffWrites.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
		middleware.RequireStaff(),
	)
	{
		staffWrites.PATCH(reportPath, handler.UpdateReport)
		staffWrites.POST("/admin/moderation/actions", handler.PerformAction)
	}
}

// registerMediaRoutes mounts the Phase 9 endpoints: upload, delete, and the
// public file route (docs/09 §27; docs/08 §22; docs/07 §23–§24).
//
//	POST /api/v1/media       Upload tier (docs/09 §31's dedicated class) +
//	                         CSRF + RequireAuth. The one route that accepts a
//	                         file, so it alone raises the body cap to the
//	                         configured media limit - every other endpoint
//	                         keeps the small global cap (docs/09 §37).
//	DELETE /api/v1/media/:id Write tier + CSRF + RequireAuth; the service
//	                         enforces owner-or-staff.
//	GET  /media/*key         the public file route, OUTSIDE /api/v1: it
//	                         serves bytes, not enveloped JSON resources, and
//	                         its path shape is the CDN origin of docs/07 §24.
//	                         PublicRead tier; the service resolves the key
//	                         through the LIVE metadata row, so deleted media
//	                         is unreachable regardless of storage state.
func registerMediaRoutes(r *gin.Engine, v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Media == nil {
		return
	}

	handler := media.NewHandler(deps.Media)

	serve := r.Group("/media")
	serve.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		serve.GET("/*"+media.KeyParam, handler.Serve)
	}

	if deps.Auth == nil {
		return
	}

	uploads := v1.Group("")
	uploads.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Upload),
		middleware.RaiseBodyLimit(deps.Media.MaxUploadBytes()+64*1024), // multipart overhead
		csrf,
		middleware.RequireAuth(),
	)
	{
		uploads.POST("/media", handler.Upload)
	}

	// The PRIVATE serve route (Phase 11): payment slips are financial evidence,
	// served ONLY here, authenticated and owner/staff-scoped in the service -
	// never through the public GET /media/*key above (addendum §9–§11). A read,
	// so it takes the PublicRead tier and no CSRF, like the other /me reads.
	privateReads := v1.Group("")
	privateReads.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.PublicRead),
		middleware.RequireAuth(),
	)
	{
		privateReads.GET("/media/:"+media.RefParam+"/private", handler.ServePrivate)
	}

	writes := v1.Group("")
	writes.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
	)
	{
		writes.DELETE("/media/:"+media.RefParam, handler.Delete)
	}
}

// registerSubscriptionRoutes mounts the Phase 11 Premium endpoints
// (docs/MONETIZATION.md). Reads are cheap; checkout/cancel are writes; the
// review/verify/reject surface is staff-only, isolated by authorization exactly
// like moderation (docs/09 §29). NONE of it carries RequireVerifiedEmail:
// subscribing is ordinary account use, not publishing.
//
//	GET  /subscription/plans     PUBLIC - a guest may browse pricing.
//	GET  /subscription           the caller's own tier/entitlements/subscription.
//	POST /subscription/checkout  Write + CSRF - start a purchase.
//	POST /subscription/cancel    Write + CSRF.
//	/admin/subscription/*        RequireStaff - the manual verification surface.
//
// A payment SLIP is uploaded through the media endpoint (purpose=payment_slip),
// not here, so this group never accepts a file.
func registerSubscriptionRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Subscription == nil {
		return
	}

	handler := subscriptions.NewHandler(deps.Subscription)
	paymentPath := "/admin/subscription/payments/:" + subscriptions.PaymentRefParam

	// --- Public pricing ----------------------------------------------------
	pub := v1.Group("")
	pub.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		pub.GET("/subscription/plans", handler.Plans)
	}

	if deps.Auth == nil {
		return
	}

	// --- The caller's own subscription -------------------------------------
	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead), middleware.RequireAuth())
	{
		reads.GET("/subscription", handler.Overview)
	}

	writes := v1.Group("")
	writes.Use(middleware.RateLimit(deps.Limiter, ratelimit.Write), csrf, middleware.RequireAuth())
	{
		writes.POST("/subscription/checkout", handler.Checkout)
		// Demo activation is authenticated + CSRF-protected like any state change
		// (brief §11); the service enforces demo-mode-only and one-per-user.
		writes.POST("/subscription/demo", handler.ActivateDemo)
		writes.POST("/subscription/cancel", handler.Cancel)
	}

	// --- Staff verification surface ----------------------------------------
	staffReads := v1.Group("")
	staffReads.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.PublicRead),
		middleware.RequireAuth(),
		middleware.RequireStaff(),
	)
	{
		staffReads.GET("/admin/subscription/payments", handler.ReviewQueue)
	}

	staffWrites := v1.Group("")
	staffWrites.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
		middleware.RequireStaff(),
	)
	{
		staffWrites.POST(paymentPath+"/verify", handler.Verify)
		staffWrites.POST(paymentPath+"/reject", handler.Reject)
	}
}

// registerProfileRoutes mounts the Phase 12E public profile read.
//
//	GET /users/:user   a username or an id. Public - no session, no CSRF.
//
// It shares the `/users/:user` segment with the follow endpoints, which is why
// both packages name the parameter the same thing. The two answer different
// questions: this one is the same for everybody, the follow endpoints are
// personal and require a session.
// The write is self-scoped and lives here rather than in a settings package
// because it edits the very row this read publishes: PATCH /me/profile takes
// no reference at all, so there is no cross-user edit path to authorize
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 1).
func registerProfileRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Profiles == nil {
		return
	}

	handler := profiles.NewHandler(deps.Profiles)

	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		reads.GET("/users/:"+profiles.RefParam, handler.Get)
		// The home page's rotating writer band (docs/WRITER-SPOTLIGHT.md).
		// Public and viewer-independent, so one cached response serves all.
		reads.GET("/writers/spotlight", handler.Spotlight)
	}

	if deps.Auth == nil {
		return
	}
	writes := v1.Group("")
	writes.Use(middleware.RateLimit(deps.Limiter, ratelimit.Write), csrf, middleware.RequireAuth())
	{
		writes.PATCH("/me/profile", handler.UpdateMine)
	}
}

// registerAuthorRoutes mounts the Phase 11 self-scoped author-profile endpoints
// (the external EasyDonate link). Both target the CALLER's own row - there is no
// path parameter and no cross-user write (addendum §4–§5).
//
//	GET /me/author-profile   the caller's own donation link.
//	PUT /me/author-profile   Write + CSRF - set or clear it.
func registerAuthorRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Authors == nil || deps.Auth == nil {
		return
	}

	handler := authors.NewHandler(deps.Authors)

	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead), middleware.RequireAuth())
	{
		reads.GET("/me/author-profile", handler.GetMine)
	}

	writes := v1.Group("")
	writes.Use(middleware.RateLimit(deps.Limiter, ratelimit.Write), csrf, middleware.RequireAuth())
	{
		writes.PUT("/me/author-profile", handler.Update)
	}
}

// registerPenNameRoutes mounts the self-scoped pen-name endpoints
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2).
//
//	GET    /me/pen-names            the caller's own identities.
//	POST   /me/pen-names            Write + CSRF - add one.
//	PATCH  /me/pen-names/:pen_name  Write + CSRF - rename, re-label, set default.
//	DELETE /me/pen-names/:pen_name  Write + CSRF - remove one.
//
// Every route targets the CALLER's own rows: `/me`, like the author profile,
// so there is no cross-user path here to authorize and none to get wrong. The
// path parameter names a pen name, and the service pairs it with the caller's
// user id in the SQL predicate - an id belonging to someone else matches
// nothing and gets the same 404 an absent one gets (docs/11 §3.4).
//
// The DELETE is a write like any other, deliberately: it removes an IDENTITY,
// never a work. The fictions published under the name keep every word and fall
// back to the writer's default (CLAUDE.md - Writer-First Principles).
func registerPenNameRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.PenNames == nil || deps.Auth == nil {
		return
	}

	handler := pennames.NewHandler(deps.PenNames)

	penNamePath := "/me/pen-names"
	onePath := penNamePath + "/:" + pennames.RefParam

	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead), middleware.RequireAuth())
	{
		reads.GET(penNamePath, handler.List)
	}

	writes := v1.Group("")
	writes.Use(middleware.RateLimit(deps.Limiter, ratelimit.Write), csrf, middleware.RequireAuth())
	{
		writes.POST(penNamePath, handler.Create)
		writes.PATCH(onePath, handler.Update)
		writes.DELETE(onePath, handler.Delete)
	}
}

// registerAchievementRoutes mounts the achievement endpoints
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3).
//
//	GET  /me/achievements            the owner: everything, with progress, and
//	                                 the trigger text of every egg they FOUND.
//	GET  /users/:user/achievements   PUBLIC: what the owner chose to showcase,
//	                                 plus counts. An egg appears here only as a
//	                                 number - never a name, whatever the owner
//	                                 chose, because naming one kills it for
//	                                 everybody.
//	PUT  /me/achievements/showcase   Write + CSRF - choose the 3-5 to display.
//	PUT  /me/achievements/prefs      Write + CSRF - the global off switch.
//	POST /achievements/signal        Write + CSRF - the four cosmetic eggs, from
//	                                 a server-side allowlist and nothing else.
//
// The public read takes no identity at all, exactly like the profile read it
// sits beside: the same bytes for a guest, a stranger, and the person
// themselves, so one cached response serves everybody.
//
// The signal endpoint carries the Write tier rather than a looser one on
// purpose. It is reachable by anyone with a session and a keyboard, and the
// things it can unlock are cosmetic by definition - so its budget should come
// out of the same allowance a writer's real writes do, not a bigger one.
func registerAchievementRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Achievements == nil {
		return
	}

	handler := achievements.NewHandler(deps.Achievements)

	public := v1.Group("")
	public.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		public.GET("/users/:"+achievements.RefParam+"/achievements", handler.Public)
	}

	if deps.Auth == nil {
		return
	}

	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead), middleware.RequireAuth())
	{
		reads.GET("/me/achievements", handler.Mine)
	}

	writes := v1.Group("")
	writes.Use(middleware.RateLimit(deps.Limiter, ratelimit.Write), csrf, middleware.RequireAuth())
	{
		writes.PUT("/me/achievements/showcase", handler.SetShowcase)
		writes.PUT("/me/achievements/prefs", handler.SetPrefs)
		writes.POST("/achievements/signal", handler.Signal)
	}
}

// registerDeskRoutes mounts the writer's shell read.
//
//	GET /me/desk   the caller's own unfinished count, words today, recent
//	               fictions, and where they stopped. Session-only: there is no
//	               id in the path, so it cannot be pointed at another writer.
//
// Read tier, not Write: every page a signed-in writer opens draws the header
// that asks for this, so it must cost what a read costs. It is deliberately a
// GET with no side effects and no CSRF - nothing here changes anything.
func registerDeskRoutes(v1 *gin.RouterGroup, deps Dependencies) {
	if deps.Desk == nil || deps.Auth == nil {
		return
	}

	handler := desk.NewHandler(deps.Desk)

	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead), middleware.RequireAuth())
	{
		reads.GET("/me/desk", handler.Mine)
	}

	// Searching one's own drafts costs what searching costs. It is a text scan
	// like the public one, and it fires on every keystroke a writer makes in
	// the header, so it belongs on the Search tier rather than the read tier.
	search := v1.Group("")
	search.Use(middleware.RateLimit(deps.Limiter, ratelimit.Search), middleware.RequireAuth())
	{
		search.GET("/me/desk/search", handler.Search)
	}
}

// registerShelfRoutes mounts the OPT-IN public bookshelves (README "Bookmarks &
// Personal Library" - bookmarks are private, collections are optional).
//
//	GET /users/:user/shelves        PUBLIC. Only shelves whose owner opted in,
//	                                and only fictions a stranger may actually
//	                                open - both filtered in SQL, in the service's
//	                                repository, never here.
//	GET /me/shelves                 the owner's own, public and private.
//	POST/PATCH/DELETE /me/shelves…  Write tier + CSRF + RequireAuth.
//
// The private shelf - `bookmarks`, owned by the library package - is NOT
// reachable through any of this and never becomes reachable by flipping a
// switch. The two are separate tables for exactly that reason.
//
// No RequireVerifiedEmail anywhere: verification gates publishing FICTION, not
// ordinary account use (docs/AUTHENTICATION.md §9).
func registerShelfRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Shelves == nil {
		return
	}

	handler := shelves.NewHandler(deps.Shelves)
	shelfPath := "/me/shelves/:" + shelves.RefParam
	itemPath := shelfPath + "/items/:" + novels.RefParam

	// --- The public read ----------------------------------------------------
	// Guest-first and viewer-independent, like the profile it appears on.
	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		reads.GET("/users/:"+shelves.UserRefParam+"/shelves", handler.ListPublic)
	}

	if deps.Auth == nil {
		return
	}

	// --- The owner's own listing -------------------------------------------
	// A read, so no CSRF; personal, so RequireAuth - the same shape every other
	// /me read has.
	mine := v1.Group("")
	mine.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead), middleware.RequireAuth())
	{
		mine.GET("/me/shelves", handler.ListMine)
	}

	// --- Owner mutations ----------------------------------------------------
	// Cookie-borne session + state change = CSRF (docs/11 §22). Ownership is
	// NOT enforced here: middleware sees a route, only the service can see who
	// owns the row behind it (docs/10 §27).
	writes := v1.Group("")
	writes.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
	)
	{
		writes.POST("/me/shelves", handler.Create)
		writes.PATCH(shelfPath, handler.Update)
		writes.DELETE(shelfPath, handler.Delete)
		writes.POST(itemPath, handler.AddItem)
		writes.DELETE(itemPath, handler.RemoveItem)
	}
}

// registerWallRoutes mounts the profile comment wall.
//
//	GET    /users/:user/wall   PUBLIC and paginated - reading what people left
//	                           someone needs no account, exactly as reading a
//	                           fiction's discussion needs none (docs/03 §27).
//	                           A wall its owner switched off answers 404
//	                           WALL_DISABLED, decided in the service.
//	POST   /users/:user/wall   Write tier + CSRF + RequireAuth. There is no
//	                           guest wall: with no fiction behind it there is
//	                           no author to review a queue, so an account is
//	                           the only workable rule.
//	DELETE /wall/:entry        Write tier + CSRF + RequireAuth. The service
//	                           allows the entry's AUTHOR or the PROFILE OWNER -
//	                           a person who cannot clear their own page does not
//	                           really own the switch either.
func registerWallRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Wall == nil {
		return
	}

	handler := wall.NewHandler(deps.Wall)
	wallPath := "/users/:" + wall.UserRefParam + "/wall"

	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		reads.GET(wallPath, handler.List)
	}

	if deps.Auth == nil {
		return
	}

	writes := v1.Group("")
	writes.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
	)
	{
		writes.POST(wallPath, handler.Post)
		writes.DELETE("/wall/:"+wall.RefParam, handler.Delete)
	}
}

// registerCharacterRoutes mounts the Phase 12A cast endpoints
// (docs/PHASE-12-STORY-DEPTH.md §12A).
//
// Reads are guest-first for the same reason the fiction itself is: a reader
// deciding whether to start a story looks at its cast before signing in
// (docs/11 §12). Nothing is enforced here - the service asks the novels service
// whether the caller may read or write the parent fiction, so the cast inherits
// the fiction's gate exactly and a private fiction's characters are invisible
// (docs/10 §27).
func registerCharacterRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Characters == nil {
		return
	}

	handler := characters.NewHandler(deps.Characters)

	novelPath := "/novels/:" + novels.RefParam
	castPath := novelPath + "/characters"
	characterPath := castPath + "/:" + characters.RefParam

	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	{
		reads.GET(castPath, handler.List)
		reads.GET(characterPath, handler.Get)
	}

	if deps.Auth == nil {
		return
	}

	writes := v1.Group("")
	writes.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
	)
	{
		writes.POST(castPath, handler.Create)
		// Registered before the :character route so "order" is never captured as
		// a character id.
		writes.PUT(castPath+"/order", handler.Reorder)
		writes.PATCH(characterPath, handler.Update)
		writes.DELETE(characterPath, handler.Delete)
		writes.PUT(characterPath+"/appearances", handler.SetAppearances)
	}
}

// registerVariableRoutes mounts the Phase 13H reader-variable endpoints
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13H).
//
// The read is PUBLIC under the fiction's own gate, because a guest reading a
// reader-insert fiction has to be asked the questions before the text makes
// sense (docs/10 §2.1). The declarations carry nothing private - the reader's
// ANSWERS never reach the server at all.
//
// The write is one PUT of the whole list. Order is the order a reader is asked
// in, so a partial update could leave two variables claiming one position.
func registerVariableRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Variables == nil {
		return
	}

	handler := variables.NewHandler(deps.Variables)
	path := "/novels/:" + novels.RefParam + "/variables"

	reads := v1.Group("")
	reads.Use(middleware.RateLimit(deps.Limiter, ratelimit.PublicRead))
	reads.GET(path, handler.List)

	if deps.Auth == nil {
		return
	}

	writes := v1.Group("")
	writes.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
	)
	writes.PUT(path, handler.Replace)
}

// registerAIRoutes mounts the Phase 10 endpoints: Thai NLP assistance
// (docs/09 §24; docs/08 §25–§26; docs/12).
//
// EVERYTHING here requires authentication - there is no guest AI, and a writer
// may only touch their OWN fiction (docs/12 §33), enforced in the service. None
// of it carries RequireVerifiedEmail: AI assistance is ordinary account use, not
// publishing (docs/AUTHENTICATION.md §9).
//
// Rate tiers follow the cost of the call (docs/09 §31, §24 "AI endpoints →
// strict limit"):
//
//	generate (spell-check, create, retry)  AI tier - each may invoke the
//	                                       provider, the expensive path
//	reads (history, poll)                  PublicRead tier - cheap keyed reads
//	decisions (decide, cancel)             Write tier - cheap state changes that
//	                                       never call the provider
//
// A per-user DAILY quota is enforced inside the service on top of these
// (docs/12 §29–§30), so a burst under the per-minute limit still cannot exhaust
// the daily budget.
func registerAIRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.AI == nil || deps.Auth == nil {
		return
	}

	handler := ai.NewHandler(deps.AI)
	requestPath := "/ai/requests/:" + ai.RequestRefParam
	suggestionPath := "/ai/suggestions/:" + ai.SuggestionRefParam

	// --- Provider-invoking calls (AI tier) ---------------------------------
	generate := v1.Group("")
	generate.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.AI),
		csrf,
		middleware.RequireAuth(),
	)
	{
		generate.POST("/ai/spell-check", handler.SpellCheck)
		generate.POST("/ai/requests", handler.CreateRequest)
		generate.POST(requestPath+"/retry", handler.Retry)
	}

	// --- Reads (cheap keyed reads, no CSRF) --------------------------------
	reads := v1.Group("")
	reads.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.PublicRead),
		middleware.RequireAuth(),
	)
	{
		reads.GET("/ai/requests", handler.ListRequests)
		reads.GET(requestPath, handler.GetRequest)
		// The quota display's read - peeks the limiter, spends nothing.
		reads.GET("/ai/usage", handler.Usage)
	}

	// --- Cheap state changes (Write tier) ----------------------------------
	writes := v1.Group("")
	writes.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
	)
	{
		writes.POST(requestPath+"/cancel", handler.Cancel)
		writes.POST(suggestionPath+"/decision", handler.DecideSuggestion)
	}

	// --- The writing tools (13Y) -------------------------------------------
	if deps.AITools == nil {
		return
	}
	tools := ai.NewToolsHandler(deps.AITools)
	novelPath := "/novels/:" + novels.RefParam
	chapterPath := novelPath + "/chapters/:" + chapters.RefParam

	// Rule-running calls share the AI tier.
	{
		generate.POST("/ai/check", tools.Check)
		generate.POST("/ai/character-check", tools.CharacterCheck)
		generate.POST("/ai/convert-chat", tools.ConvertChat)
		generate.POST("/ai/continuity", tools.Continuity)
		generate.POST("/ai/precheck", tools.Precheck)
	}
	// Cheap keyed reads.
	{
		reads.GET("/ai/prefs", tools.GetPrefs)
		reads.GET("/ai/mutes", tools.ListMutes)
		reads.GET("/ai/lexicon", tools.UserLexicon)
		reads.GET(novelPath+"/lexicon", tools.Lexicon)
		reads.GET(chapterPath+"/facts", tools.Facts)
		reads.GET(novelPath+"/search", tools.Search)
	}
	// Cheap state changes.
	{
		writes.PUT("/ai/prefs", tools.SetPrefs)
		writes.POST("/ai/mutes", tools.AddMute)
		writes.DELETE("/ai/mutes/:"+ai.MuteRefParam, tools.RemoveMute)
		writes.PUT("/ai/character-evolution", tools.SetEvolution)
		// The ACCOUNT word bank (no fiction in the path).
		writes.POST("/ai/lexicon", tools.AddUserLexiconTerm)
		writes.DELETE("/ai/lexicon/:"+ai.LexiconRefParam, tools.RemoveUserLexiconTerm)
		writes.POST(novelPath+"/lexicon", tools.AddLexiconTerm)
		writes.DELETE(novelPath+"/lexicon/:"+ai.LexiconRefParam, tools.RemoveLexiconTerm)
		writes.PUT(chapterPath+"/facts", tools.SetFacts)
	}
}

// registerLibraryRoutes mounts the reader's shelf: bookmarks, follows, and
// reading progress (docs/09 §17–§19, docs/03 §13).
//
// EVERYTHING here requires authentication - there is no such thing as a guest
// shelf; guests keep reading state on their own device (docs/03 §11). None of
// it carries RequireVerifiedEmail: verification gates publishing, never
// ordinary account use (docs/AUTHENTICATION.md §9).
//
// The public reading path is untouched by this function - reads of novels and
// chapters stay in registerPublishingRoutes with no authentication of any kind.
func registerLibraryRoutes(v1 *gin.RouterGroup, deps Dependencies, csrf gin.HandlerFunc) {
	if deps.Library == nil || deps.Auth == nil {
		return
	}

	handler := library.NewHandler(deps.Library)

	novelPath := "/novels/:" + novels.RefParam
	userPath := "/users/:" + library.UserRefParam

	// --- Authenticated reads -----------------------------------------------
	// Reads change nothing, so no CSRF; they are personal, so RequireAuth.
	reads := v1.Group("")
	reads.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.PublicRead),
		middleware.RequireAuth(),
	)
	{
		reads.GET("/me/library", handler.Library)
		reads.GET("/me/reading-progress", handler.ContinueReading)
		reads.GET("/me/following", handler.Following)
		reads.GET(novelPath+"/bookmark", handler.BookmarkStatus)
		reads.GET(novelPath+"/reaction", handler.LikeStatus)
		reads.GET(novelPath+"/progress", handler.GetProgress)
		reads.GET(userPath+"/follow-status", handler.FollowStatus)

		// The library redesign's reads (library review 2026-08). History is
		// owner-only BY ROUTE - there is no public variant to mis-scope
		// (README: "never be exposed through public APIs").
		reads.GET("/me/finished", handler.Finished)
		reads.GET("/me/history", handler.History)
		reads.GET("/me/history/settings", handler.HistorySettings)
	}

	// --- Shelf mutations ---------------------------------------------------
	// Cookie-borne session + state change = CSRF (docs/11 §22).
	writes := v1.Group("")
	writes.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Write),
		csrf,
		middleware.RequireAuth(),
	)
	{
		writes.POST(novelPath+"/bookmark", handler.Bookmark)
		writes.DELETE(novelPath+"/bookmark", handler.Unbookmark)
		writes.POST(novelPath+"/reaction", handler.Like)
		writes.DELETE(novelPath+"/reaction", handler.Unlike)
		writes.POST(userPath+"/follow", handler.Follow)
		writes.DELETE(userPath+"/follow", handler.Unfollow)

		// The library redesign's writes (library review 2026-08).
		writes.PATCH(userPath+"/follow", handler.SetFollowNotify)
		writes.DELETE(novelPath+"/progress", handler.DeleteProgress)
		writes.PUT(novelPath+"/finished", handler.MarkFinished)
		writes.DELETE(novelPath+"/finished", handler.UnmarkFinished)
		writes.DELETE("/me/history", handler.ClearHistory)
		writes.PUT("/me/history/settings", handler.SetHistorySettings)
	}

	// --- Progress saves ----------------------------------------------------
	// The one high-frequency write, on its own rate-limit class so a reader's
	// debounced saves never consume the Write quota (docs/09 §17, §31).
	progress := v1.Group("")
	progress.Use(
		middleware.RateLimit(deps.Limiter, ratelimit.Progress),
		csrf,
		middleware.RequireAuth(),
	)
	{
		progress.PUT(novelPath+"/progress", handler.SaveProgress)
	}
}

// Command server runs the FictionThai API.
//
// The API is a modular monolith: one deployable binary, internally divided into
// independent domains (docs/07 - System Architecture.md §7). This entrypoint
// wires infrastructure and hands it to the router; it contains no business
// logic itself.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	"github.com/fictionthai/fictionthai/backend/internal/insights"
	"github.com/fictionthai/fictionthai/backend/internal/library"
	"github.com/fictionthai/fictionthai/backend/internal/media"
	"github.com/fictionthai/fictionthai/backend/internal/moderation"
	"github.com/fictionthai/fictionthai/backend/internal/notifications"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/pennames"
	"github.com/fictionthai/fictionthai/backend/internal/platform/cache"
	"github.com/fictionthai/fictionthai/backend/internal/platform/database"
	"github.com/fictionthai/fictionthai/backend/internal/platform/email"
	"github.com/fictionthai/fictionthai/backend/internal/platform/storage"
	"github.com/fictionthai/fictionthai/backend/internal/profiles"
	"github.com/fictionthai/fictionthai/backend/internal/promo"
	"github.com/fictionthai/fictionthai/backend/internal/ratelimit"
	"github.com/fictionthai/fictionthai/backend/internal/server"
	"github.com/fictionthai/fictionthai/backend/internal/shelves"
	"github.com/fictionthai/fictionthai/backend/internal/subscriptions"
	"github.com/fictionthai/fictionthai/backend/internal/taxonomy"
	"github.com/fictionthai/fictionthai/backend/internal/users"
	"github.com/fictionthai/fictionthai/backend/internal/variables"
	"github.com/fictionthai/fictionthai/backend/internal/views"
	"github.com/fictionthai/fictionthai/backend/internal/wall"
	"github.com/fictionthai/fictionthai/backend/pkg/logger"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)"
var version = "dev"

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if configuration failed, so report the
		// fatal error on stderr and exit non-zero for the supervisor.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logger.New(cfg.App.Env, cfg.App.LogLevel)
	log.Info("starting fictionthai api",
		slog.String("version", version),
		slog.Any("config", cfg.Redacted()),
	)

	// Cancelled on SIGINT/SIGTERM, which begins graceful shutdown (docs/14 §46).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer closeQuietly(log, "postgres", db.Close)
	log.Info("connected to postgres")

	redis, err := cache.Connect(ctx, cfg.Redis)
	if err != nil {
		return fmt.Errorf("connect to redis: %w", err)
	}
	defer closeQuietly(log, "redis", redis.Close)
	if redis.Enabled() {
		log.Info("connected to redis")
	} else {
		log.Info("redis is not configured; running without cache",
			slog.String("detail", "set REDIS_URL to enable"))
	}

	limiter := newLimiter(cfg, redis, log)
	defer closeQuietly(log, "rate limiter", limiter.Close)

	// Identity and authentication (docs/09 §47 Phase 1).
	userRepo := users.NewRepository(db.DB)
	authService := auth.NewService(
		userRepo,
		auth.NewSessionRepository(db.DB),
		auth.NewTokenRepository(db.DB),
		newMailer(cfg, log),
		log,
		auth.Config{
			WebLifetime: auth.Lifetime{
				Absolute: cfg.Session.WebAbsoluteLifetime,
				Idle:     cfg.Session.WebIdleTimeout,
			},
			MobileLifetime: auth.Lifetime{
				Absolute: cfg.Session.MobileAbsoluteLifetime,
				Idle:     cfg.Session.MobileIdleTimeout,
			},
			PasswordParams:       auth.DefaultPasswordParams(),
			PasswordResetTTL:     cfg.Session.PasswordResetTTL,
			EmailVerificationTTL: cfg.Session.EmailVerificationTTL,
			AppURL:               cfg.Session.AppURL,
			TouchInterval:        cfg.Session.TouchInterval,
		},
	)

	// Interaction infrastructure (docs/08 §44 Phase 6, docs/07 §27, §37).
	//
	// Notifications are built FIRST because publishing domains emit into them
	// through their consumer-defined Notifier interfaces. The queue prefers
	// Redis (survives restarts, shared across instances); the in-process
	// channel is the documented development fallback, exactly like the rate
	// limiter (docs/07 §18).
	notificationQueue := newNotificationQueue(cfg, redis, log)
	notificationRepo := notifications.NewRepository(db.DB)
	notificationService := notifications.NewService(notificationRepo, notificationQueue, log)

	// The worker runs inside the API process - one deployable, per the modular
	// monolith (docs/07 §7). It stops with the same signal the server does and
	// is given a moment to finish an in-flight delivery.
	worker := notifications.NewWorker(notificationQueue, notificationRepo, log)
	waitForWorker := worker.Start(ctx)
	defer waitForWorker()

	// Publishing core (docs/09 §47 Phase 1, docs/08 §44 Phases 2-3).
	//
	// Chapters depend on novels for ownership, never the reverse: a chapter's
	// owner is reached through its fiction (docs/08 §10.2).
	novelRepo := novels.NewRepository(db.DB)
	taxonomyRepo := taxonomy.NewRepository(db.DB)
	taxonomyService := taxonomy.NewService(taxonomyRepo, log)
	// The library repository is constructed FIRST because the novel service now
	// needs one fact from it: whether a viewer follows an author, which is the
	// followers-only rung of the visibility ladder (§13C). The dependency runs
	// novels -> library through a consumer-defined interface; library keeps
	// importing novels for the shared SQL predicates, and the two never form a
	// package cycle because only one direction is an import.
	libraryRepo := library.NewRepository(db.DB)
	novelService := novels.NewService(novelRepo, userRepo, taxonomyRepo, libraryRepo, log)
	chapterRepo := chapters.NewRepository(db.DB)
	chapterService := chapters.NewService(chapterRepo, novelService, notificationService, log)
	libraryService := library.NewService(libraryRepo, novelService, novelRepo, userRepo, notificationService, log)
	commentRepo := comments.NewRepository(db.DB)
	commentService := comments.NewService(commentRepo, novelService, chapterService, notificationService, log)

	// Cast layer (Phase 12A, docs/PHASE-12-STORY-DEPTH.md §12A). Takes the novel
	// service as its gate, so a character is exactly as reachable as the fiction
	// it belongs to.
	characterService := characters.NewService(characters.NewRepository(db.DB), novelService, log)
	variableService := variables.NewService(variables.NewRepository(db.DB), novelService, log)

	// The home hero's slide queue (docs/HOME-PROMO.md).
	promoService := promo.NewService(promo.NewRepository(db.DB), log)

	// Public profiles (Phase 12E). A read-only composition of identity and
	// published work; it owns no writes and takes no identity.
	profileService := profiles.NewService(profiles.NewRepository(db.DB), log)

	// Pen names (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2): the identities a
	// writer publishes under. Self-scoped like the author profile - every call
	// resolves against the caller's own rows, and removing an identity never
	// removes a word of the work published under it.
	penNameService := pennames.NewService(pennames.NewRepository(db.DB), log)

	// Public bookshelves and the profile wall. Shelves take the novels service
	// as their gate - a fiction can only be shelved by someone who may read it -
	// and the novels repository for the cards, so `novels` stays the single
	// source of truth for what a card contains. Neither service can see
	// `bookmarks`: the private shelf stays private (README, library package doc).
	shelfService := shelves.NewService(
		shelves.NewRepository(db.DB), novelService, novelRepo, log)
	wallService := wall.NewService(wall.NewRepository(db.DB), log)

	// Read counting (Phase 12C). Redis absorbs the per-read work and a flusher
	// applies the totals in batches, so opening a chapter never takes a database
	// write. With Redis unavailable it counts nothing rather than failing reads.
	viewRepo := views.NewRepository(db.DB)
	viewRecorder := views.NewRecorder(redis.Redis(), viewRepo, log)
	if viewRecorder.Enabled() {
		go viewRecorder.Run(ctx)
	} else {
		log.Warn("view counting is disabled: Redis is unavailable")
	}

	// Community layer (docs/08 §44 Phase 7): a separate domain from fiction,
	// sharing only the users lookup and the notification pipeline - plus, since
	// Phase 12D, read-only access to fictions and chapters so a post can attach
	// one. The dependency runs one way: nothing in the fiction domain knows
	// that community exists.
	communityRepo := community.NewRepository(db.DB)
	communityService := community.NewService(
		communityRepo, userRepo,
		novelService, chapterService,
		notificationService, log,
	)

	// The studio overview (§13R). It sits ABOVE the domains it reads and owns no
	// table: the ownership check is the novels service's own, and every source
	// below it is that domain's repository behind a consumer-defined interface.
	// The one-way rule holds - insights knows about community, and community
	// still does not know this exists.
	insightsService := insights.NewService(
		novelService, viewRepo, commentRepo, communityRepo, log,
	)

	// The writer's shell (the navbar's own read). insights answers about ONE
	// fiction; this answers about the caller's whole desk, from the same two
	// repositories, in one request per page rather than four.
	deskService := desk.NewService(chapterRepo, novelRepo)

	// Premium subscription layer (docs/08 §44 Phase 11, docs/MONETIZATION.md):
	// platform-owned Premium/Pro. Built BEFORE media because a payment-slip upload
	// attaches through this service (media.PaymentSlipTarget). It never touches
	// writer money - that is the separate, external EasyDonate link (authors).
	subscriptionService := subscriptions.NewService(
		subscriptions.NewRepository(db.DB), notificationService,
		subscriptions.Config{
			Mode:            subscriptions.Mode(cfg.Subscription.Mode),
			PromptPayTarget: cfg.Subscription.PromptPayTarget,
			PromptPayName:   cfg.Subscription.PromptPayName,
			DemoTier:        subscriptions.Tier(cfg.Subscription.DemoTier),
			DemoDuration:    time.Duration(cfg.Subscription.DemoDurationDays) * 24 * time.Hour,
		},
		log,
	)

	// Author-profile writes (Phase 11): the first write path for author_profiles,
	// used for the external writer-support (EasyDonate) link.
	authorService := authors.NewService(authors.NewRepository(db.DB), log)

	// Media layer (docs/08 §44 Phase 9): upload metadata and lifecycle. The
	// bytes live behind the storage boundary - the local filesystem backend
	// in development, an S3-compatible backend later (docs/07 §22, §65). The
	// subscription service is passed as the private payment-slip target.
	objectStore, err := storage.NewLocal(cfg.Media.StoragePath)
	if err != nil {
		return fmt.Errorf("initialise media storage: %w", err)
	}
	mediaService := media.NewService(
		media.NewRepository(db.DB), objectStore, novelService, userRepo, subscriptionService,
		media.Config{
			MaxUploadBytes: cfg.Media.MaxUploadBytes,
			PublicBaseURL:  cfg.Media.PublicBaseURL,
		},
		log,
	)

	// Moderation layer (docs/08 §44 Phase 8): reports and the audit trail.
	// Every state change is delegated back to the owning domain's service -
	// this wiring is where those narrow interfaces meet their implementations.
	moderationService := moderation.NewService(
		moderation.NewRepository(db.DB),
		novelService, chapterService, commentService, communityService,
		userRepo, mediaService, notificationService, log,
	)

	// AI / Thai NLP layer (docs/08 §44 Phase 10). The language work lives behind
	// the provider boundary - the local, deterministic Thai rule engine today; a
	// self-hosted model or an external LLM later (docs/12 §7, §26). AI is
	// OPTIONAL infrastructure: the async worker mirrors the notifications worker
	// (one process, docs/07 §7), and when AI is switched off nothing else changes
	// (docs/12 §31).
	aiQueue := newAIQueue(cfg, redis, log)
	aiService := ai.NewService(
		ai.NewRepository(db.DB), ai.NewLocalProvider(), chapterService, notificationService,
		aiQueue, limiter,
		ai.Config{
			Enabled:       cfg.AI.Enabled,
			MaxInputRunes: cfg.AI.MaxInputRunes,
			DailyQuota:    cfg.AI.DailyQuota,
		},
		log,
	)
	// The 13Y writing tools share the provider and config but carry their own
	// dependencies (the word bank derives from characters and variables).
	aiTools := ai.NewTools(
		ai.NewToolsRepository(db.DB), ai.NewLocalProvider(),
		novelService, characterService, variableService,
		ai.Config{
			Enabled:       cfg.AI.Enabled,
			MaxInputRunes: cfg.AI.MaxInputRunes,
			DailyQuota:    cfg.AI.DailyQuota,
			ModelURL:      cfg.AI.ModelURL,
		},
		log,
	)

	// Achievements (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3). Built LAST among
	// the domains, because it is the only one they all point at: each of them
	// declares its own one-method Achiever interface and this is the single
	// implementation, so the dependency runs domain -> achievements and never
	// back. Attached by setter rather than by constructor deliberately - the
	// signal is an afterthought at every choke point by design, and a service
	// that never gets one behaves exactly as it did before.
	achievementService := achievements.NewService(achievements.NewRepository(db.DB), log)
	chapterService.SetAchiever(achievementService)
	novelService.SetAchiever(achievementService)
	characterService.SetAchiever(achievementService)
	aiService.SetAchiever(achievementService)
	aiTools.SetAchiever(achievementService)

	if cfg.AI.Enabled {
		aiWorker := ai.NewWorker(aiQueue, aiService, log)
		// Re-drive durable jobs from a previous run BEFORE consuming new work
		// (docs/12 §28). Single-instance recovery, matching the deployment model.
		aiWorker.Recover(ctx)
		waitForAIWorker := aiWorker.Start(ctx)
		defer waitForAIWorker()
	} else {
		log.Info("ai assistance is disabled",
			slog.String("detail", "set AI_ENABLED=true to enable"))
	}

	router := server.NewRouter(server.Dependencies{
		Config:        cfg,
		Logger:        log,
		DB:            db,
		Cache:         redis,
		Limiter:       limiter,
		Version:       version,
		Auth:          authService,
		Novels:        novelService,
		Chapters:      chapterService,
		Insights:      insightsService,
		Desk:          deskService,
		Library:       libraryService,
		Taxonomy:      taxonomyService,
		Comments:      commentService,
		Notifications: notificationService,
		Community:     communityService,
		Moderation:    moderationService,
		Media:         mediaService,
		AI:            aiService,
		AITools:       aiTools,
		Subscription:  subscriptionService,
		Authors:       authorService,
		Characters:    characterService,
		Variables:     variableService,
		Promo:         promoService,
		Profiles:      profileService,
		PenNames:      penNameService,
		Shelves:       shelfService,
		Wall:          wallService,
		Achievements:  achievementService,
		Views:         viewRecorder,
	})

	return server.New(cfg, router, log).Run(ctx)
}

// newAIQueue prefers the Redis-backed AI job queue so queued jobs survive a
// restart and are shared across instances (docs/09 §46, docs/12 §27). Without
// Redis the in-process queue keeps AI working in development; the AI worker's
// startup recovery re-drives any rows left queued, so a memory-queue restart is
// not silently lossy. Choosing the fallback outside development is worth a
// warning, like the notification queue and rate limiter (docs/07 §18).
func newAIQueue(cfg *config.Config, redis *cache.Client, log *slog.Logger) ai.Queue {
	if redis.Enabled() {
		return ai.NewRedisQueue(redis.Redis())
	}
	if !cfg.App.IsDevelopment() && cfg.App.Env != config.EnvTest {
		log.Warn("using in-process ai job queue without redis",
			slog.String("impact", "queued jobs are re-driven from the database on restart, not shared across instances"))
	}
	return ai.NewMemoryQueue()
}

// newNotificationQueue prefers the Redis-backed queue so events survive a
// restart and are shared across instances (docs/09 §46). Without Redis the
// in-process queue keeps notifications working in development; outside
// development that trade-off deserves a warning, like the rate limiter's.
func newNotificationQueue(cfg *config.Config, redis *cache.Client, log *slog.Logger) notifications.Queue {
	if redis.Enabled() {
		return notifications.NewRedisQueue(redis.Redis())
	}
	if !cfg.App.IsDevelopment() && cfg.App.Env != config.EnvTest {
		log.Warn("using in-process notification queue without redis",
			slog.String("impact", "queued events are lost on restart and not shared across instances"))
	}
	return notifications.NewMemoryQueue()
}

// newLimiter prefers the Redis-backed limiter so the budget is shared across
// API instances. The in-memory fallback is correct only for a single instance,
// so choosing it outside development is worth a warning (docs/14 §40).
func newLimiter(cfg *config.Config, redis *cache.Client, log *slog.Logger) ratelimit.Limiter {
	if redis.Enabled() {
		return ratelimit.NewRedisLimiter(redis.Redis())
	}
	if !cfg.App.IsDevelopment() && cfg.App.Env != config.EnvTest {
		log.Warn("using in-process rate limiting without redis",
			slog.String("impact", "limits are per API instance, not global"))
	}
	return ratelimit.NewMemoryLimiter()
}

// newMailer selects the outbound mail transport.
//
// Only development transports exist so far. config.validateEmail refuses to
// start outside development with the log transport, so this cannot silently
// write password-reset links into a production log.
func newMailer(cfg *config.Config, log *slog.Logger) email.Sender {
	switch cfg.Email.Transport {
	case config.EmailTransportDiscard:
		return email.DiscardSender{}
	default:
		return email.NewLogSender(log)
	}
}

// closeQuietly logs a shutdown failure instead of masking the original error.
func closeQuietly(log *slog.Logger, name string, closeFn func() error) {
	if err := closeFn(); err != nil {
		log.Error("failed to close resource",
			slog.String("resource", name),
			slog.Any("error", err),
		)
	}
}

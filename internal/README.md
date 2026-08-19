# Backend Domain Boundaries

The API is a **modular monolith**: one deployable binary, internally divided
into independent domains (`docs/07 - System Architecture.md` §7).

## Layering

Every domain follows the same four layers. This is not decoration - it is what
keeps authorization enforceable and testable.

```
HTTP Handler        parse and validate the request shape, render the response
      ↓
Service             business rules, authorization, transactions, events
      ↓
Repository          data access; the ONLY layer that writes SQL
      ↓
PostgreSQL
```

Rules that follow from this:

- Handlers hold no business logic. A handler that needs to decide something
  belongs in a service.
- SQL never appears outside a repository. Scattered queries make an index or an
  N+1 problem impossible to reason about (`docs/14` §13).
- **Ownership checks live in the service, not in the route declaration.**
  `docs/10` §27 is explicit: a check that exists only in middleware is bypassed
  the moment a second endpoint calls the same service.
- Slow work - AI calls, notifications, e-mail - never runs inside a database
  transaction (`docs/09` §45).

## Package layout

One package per domain, created when it is implemented - empty directories
are not boundaries.

| Package | Responsibility |
|---|---|
| `config` | Environment configuration and validation |
| `server` | Router, middleware wiring, HTTP lifecycle |
| `middleware` | Request ID, logging, recovery, CORS, security headers, body limit, rate limiting |
| `platform/database` | PostgreSQL pool and the migration runner |
| `platform/cache` | Redis client |
| `health` | Liveness and readiness probes |
| `ratelimit` | Tiered rate-limit policies, Redis and in-memory limiters |
| `auth` | Credentials and sessions |
| `users` | The identity record and its public profile |
| `fiction` | **Fiction Format System** - the three format dimensions and their rules |
| `novels` | The Fiction entity: lifecycle, ownership, format metadata |
| `chapters` | Chapter content - prose, chat messages, headcanon entries, revisions |
| `characters` | A fiction's cast |
| `taxonomy` | The discovery vocabularies: genres and tags (two separate vocabularies) |
| `library` | The reader's personal shelf: bookmarks, follows, reading progress |
| `shelves` | Opt-in public collections of other people's fiction |
| `comments` | Reader discussion on fictions and chapters |
| `community` | Short posts, comment threads, and reactions - a domain separate from fiction |
| `wall` | The profile comment wall |
| `notifications` | Per-user notification delivery |
| `moderation` | User reports and the moderator audit trail |
| `media` | Uploaded-file metadata and lifecycle |
| `ai` | Optional Thai NLP assistance - assistive only, never edits a manuscript itself |
| `variables` | Reader variables: tokens a fiction declares and a reader answers |
| `authors` | The author-profile write surface |
| `pennames` | The identities a writer publishes under |
| `profiles` | The reader/writer profile write surface |
| `achievements` | Awards, tallies, and the switch that turns them off |
| `insights` | The studio overview's per-fiction statistics |
| `desk` | The writer shell every studio page shares |
| `promo` | Home-page promotional slides |
| `views` | Buffered view-count application |
| `subscriptions` | The platform-owned Premium/Pro monetization stream |

`pkg/` holds cross-domain utilities with no business logic: `apierror`,
`response`, `pagination`, `logger`.

## The Fiction Format System

`internal/fiction` is the single definition of what a fiction's format is. It is
deliberately free of database and HTTP dependencies so the novels service, the
chapters service, validation, and tests all share one source of truth.

Three **independent** dimensions (`docs/08` §2):

| Dimension | Values | Question it answers |
|---|---|---|
| `story_structure` | `one_shot`, `multi_chapter` | How is the work organised? |
| `presentation_format` | `standard`, `chat` | How is it rendered to readers? |
| `content_mode` | `general`, `headcanon` | How does the author classify it? |

All eight combinations are valid. Do not collapse them into one `type` enum
(`docs/08` §43 Rule 6) and do not restrict a combination for implementation
convenience (`docs/08` §2.4).

**The invariant that matters most:** changing a format changes metadata only. It
must never delete chapters, rewrite prose into chat messages, merge or split
chapters, or drop comments, bookmarks, or reading progress
(`docs/08` §3.1, `docs/09` §14.6, `docs/15` §5.7). If content conversion is ever
offered, it is a separate, explicit author action.

## Adding a domain

1. Read the relevant documents in `docs/` first - they are the source of truth.
2. Create `internal/<domain>/` with `handler.go`, `service.go`, `repository.go`.
3. Add migrations under `backend/migrations/`, following `docs/08` §44 order.
4. Register the route group in `internal/server/router.go` with the rate-limit
   tier appropriate to the endpoint class (`docs/09` §31).
5. Answer every question in `docs/09` §50 before considering the endpoint done.
6. Write tests at the layer the risk lives in - `docs/15` §49 ranks
   authorization, ownership, and format integrity above everything else.

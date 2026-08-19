package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/fictionthai/fictionthai/backend/internal/ratelimit"
)

func TestMemoryLimiter_AllowsUpToLimitThenDenies(t *testing.T) {
	limiter := ratelimit.NewMemoryLimiter()
	t.Cleanup(func() { _ = limiter.Close() })

	policy := ratelimit.Policy{Name: "test", Limit: 3, Window: time.Minute}
	ctx := context.Background()

	for i := 1; i <= policy.Limit; i++ {
		res := limiter.Allow(ctx, "1.2.3.4", policy)
		if !res.Allowed {
			t.Fatalf("request %d should have been allowed", i)
		}
		if want := policy.Limit - i; res.Remaining != want {
			t.Errorf("request %d remaining = %d, want %d", i, res.Remaining, want)
		}
	}

	res := limiter.Allow(ctx, "1.2.3.4", policy)
	if res.Allowed {
		t.Fatal("the request past the limit should have been denied")
	}
	if res.RetryAfter <= 0 {
		t.Error("a denied result must tell the client when to retry")
	}
}

// Peek is what the quota display reads: looking at "เหลือเท่าไหร่" must never
// spend any of it.
func TestMemoryLimiter_PeekSpendsNothing(t *testing.T) {
	limiter := ratelimit.NewMemoryLimiter()
	t.Cleanup(func() { _ = limiter.Close() })

	policy := ratelimit.Policy{Name: "peek", Limit: 3, Window: time.Minute}
	ctx := context.Background()

	// An untouched key reads as a full budget.
	if res := limiter.Peek(ctx, "u1", policy); !res.Allowed || res.Remaining != policy.Limit {
		t.Fatalf("peek before any hit = %+v, want full budget", res)
	}

	limiter.Allow(ctx, "u1", policy)
	limiter.Allow(ctx, "u1", policy)

	// Peek reports the standing and, however often it is called, changes nothing.
	for i := 0; i < 5; i++ {
		if res := limiter.Peek(ctx, "u1", policy); res.Remaining != 1 {
			t.Fatalf("peek after 2 hits = %+v, want remaining 1", res)
		}
	}
	if res := limiter.Allow(ctx, "u1", policy); !res.Allowed {
		t.Fatal("the third hit should still be allowed - peeks must not have counted")
	}
	if res := limiter.Peek(ctx, "u1", policy); res.Allowed || res.Remaining != 0 {
		t.Fatalf("peek at the limit = %+v, want denied with 0 remaining", res)
	}
}

func TestMemoryLimiter_IsolatesKeys(t *testing.T) {
	limiter := ratelimit.NewMemoryLimiter()
	t.Cleanup(func() { _ = limiter.Close() })

	policy := ratelimit.Policy{Name: "test", Limit: 1, Window: time.Minute}
	ctx := context.Background()

	limiter.Allow(ctx, "reader-a", policy)

	// One noisy client must never rate-limit a different client.
	if res := limiter.Allow(ctx, "reader-b", policy); !res.Allowed {
		t.Error("a different key must have its own budget")
	}
	if res := limiter.Allow(ctx, "reader-a", policy); res.Allowed {
		t.Error("the exhausted key should still be denied")
	}
}

func TestMemoryLimiter_IsolatesPolicies(t *testing.T) {
	limiter := ratelimit.NewMemoryLimiter()
	t.Cleanup(func() { _ = limiter.Close() })

	strict := ratelimit.Policy{Name: "auth", Limit: 1, Window: time.Minute}
	relaxed := ratelimit.Policy{Name: "public_read", Limit: 1, Window: time.Minute}
	ctx := context.Background()

	limiter.Allow(ctx, "same-ip", strict)

	// Exhausting the auth budget must not lock the same visitor out of reading.
	if res := limiter.Allow(ctx, "same-ip", relaxed); !res.Allowed {
		t.Error("policies must hold independent counters")
	}
}

func TestMemoryLimiter_WindowExpires(t *testing.T) {
	limiter := ratelimit.NewMemoryLimiter()
	t.Cleanup(func() { _ = limiter.Close() })

	policy := ratelimit.Policy{Name: "test", Limit: 1, Window: 20 * time.Millisecond}
	ctx := context.Background()

	limiter.Allow(ctx, "key", policy)
	if res := limiter.Allow(ctx, "key", policy); res.Allowed {
		t.Fatal("expected the second request in the window to be denied")
	}

	time.Sleep(30 * time.Millisecond)

	if res := limiter.Allow(ctx, "key", policy); !res.Allowed {
		t.Error("traffic should be allowed again after the window elapses")
	}
}

// Every documented policy must be usable, so a typo in a policy definition
// surfaces here rather than in production.
func TestDocumentedPoliciesAreSane(t *testing.T) {
	policies := []ratelimit.Policy{
		ratelimit.PublicRead, ratelimit.Search, ratelimit.Auth,
		ratelimit.Write, ratelimit.AI, ratelimit.Upload,
	}
	seen := map[string]bool{}

	for _, p := range policies {
		if p.Name == "" {
			t.Error("every policy needs a name; it is part of the counter key")
		}
		if seen[p.Name] {
			t.Errorf("duplicate policy name %q would share a counter", p.Name)
		}
		seen[p.Name] = true

		if p.Limit <= 0 {
			t.Errorf("policy %q has limit %d, which would block all traffic", p.Name, p.Limit)
		}
		if p.Window <= 0 {
			t.Errorf("policy %q has a non-positive window", p.Name)
		}
	}

	// docs/09 §31: public reading gets the highest allowance, AI the strictest.
	if ratelimit.PublicRead.Limit <= ratelimit.AI.Limit {
		t.Error("public reading must be more permissive than AI endpoints")
	}
	if ratelimit.Auth.Limit >= ratelimit.PublicRead.Limit {
		t.Error("authentication must be stricter than public reading")
	}
}

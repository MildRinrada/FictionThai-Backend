// Package health implements the liveness and readiness probes.
//
// docs/14 - Infrastructure & Deployment.md §45 draws the distinction:
//
//	/health - is the process alive?      (never touches dependencies)
//	/ready  - are dependencies usable?   (checks PostgreSQL and Redis)
//
// Keeping them separate matters operationally: a load balancer must not restart
// a healthy process just because the database is briefly unreachable.
package health

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Status values reported by a dependency check.
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StatusDisabled = "disabled"
)

// Checker is one named dependency probe.
type Checker interface {
	// Name identifies the dependency in the readiness payload.
	Name() string
	// Check returns nil when the dependency is usable.
	Check(ctx context.Context) error
	// Enabled reports whether the dependency is configured for this
	// deployment. A disabled dependency is reported, not failed - Redis is
	// optional (docs/07 §18).
	Enabled() bool
}

// Handler serves the probes.
type Handler struct {
	version  string
	checkers []Checker
	// timeout bounds the whole readiness check so a hung dependency cannot hold
	// a probe connection open indefinitely.
	timeout time.Duration
}

// NewHandler builds the probe handler.
func NewHandler(version string, checkers ...Checker) *Handler {
	return &Handler{version: version, checkers: checkers, timeout: 3 * time.Second}
}

// Live handles GET /health. It answers only "is this process running?", so it
// performs no I/O and always returns 200 while the process can serve.
func (h *Handler) Live(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": StatusOK})
}

type readyResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version"`
	Checks  map[string]string `json:"checks"`
}

// Ready handles GET /ready. It probes every dependency concurrently and returns
// 503 when any *enabled* dependency is unusable, so an orchestrator stops
// routing traffic to an instance that cannot serve it.
func (h *Handler) Ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		checks  = make(map[string]string, len(h.checkers))
		healthy = true
	)

	for _, checker := range h.checkers {
		wg.Add(1)
		go func(ck Checker) {
			defer wg.Done()

			status := StatusOK
			if !ck.Enabled() {
				status = StatusDisabled
			} else if err := ck.Check(ctx); err != nil {
				status = StatusDegraded
			}

			mu.Lock()
			checks[ck.Name()] = status
			if status == StatusDegraded {
				healthy = false
			}
			mu.Unlock()
		}(checker)
	}
	wg.Wait()

	body := readyResponse{Status: StatusOK, Version: h.version, Checks: checks}
	status := http.StatusOK
	if !healthy {
		body.Status = StatusDegraded
		status = http.StatusServiceUnavailable
	}

	// A probe response must never be cached by an intermediary.
	c.Header("Cache-Control", "no-store")
	c.JSON(status, body)
}

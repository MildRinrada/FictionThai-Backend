// Package email provides the outbound mail abstraction.
//
// No production provider is integrated yet. The interface exists so the
// authentication domain can be finished now and a provider chosen later without
// touching it - docs/14 §64 keeps infrastructure decisions replaceable, and
// docs/07 §65 states the principle: modular internally, replaceable externally.
package email

import (
	"context"
	"log/slog"
)

// Message is one outbound email.
//
// It carries a rendered subject and body rather than a template name, so the
// domain decides what to say and this package only decides how to deliver it.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Sender delivers messages.
//
// Implementations must treat delivery failure as non-fatal to the caller's
// operation where the documentation requires it - a password-reset response
// must look identical whether or not the address exists (docs/10 §16), which
// means the API cannot surface a send failure for a missing account.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// LogSender is the development implementation.
//
// It writes the message to the application log instead of contacting a provider,
// so a developer can complete a reset or verification flow locally with no
// external service and no credentials configured.
//
// It never makes a network request - docs/14 §32 forbids development software
// reaching production services by accident.
type LogSender struct {
	log *slog.Logger
}

func NewLogSender(log *slog.Logger) *LogSender { return &LogSender{log: log} }

func (s *LogSender) Send(ctx context.Context, msg Message) error {
	// The body contains a single-use link. That is acceptable in a development
	// log - it is the only way to complete the flow without a provider - but it
	// is exactly why LogSender must never be selected outside development.
	// Config.validate enforces that.
	s.log.InfoContext(ctx, "email (development transport, not delivered)",
		slog.String("event", "email_send"),
		slog.String("to", msg.To),
		slog.String("subject", msg.Subject),
		slog.String("body", msg.Body),
	)
	return nil
}

// DiscardSender drops every message. Used in tests, where asserting on a mailbox
// is not the point and log noise is unhelpful.
type DiscardSender struct{}

func (DiscardSender) Send(context.Context, Message) error { return nil }

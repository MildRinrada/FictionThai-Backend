// Package users owns the identity record and its public profile.
//
// It deliberately knows nothing about sessions, cookies, or passwords beyond
// storing an already-hashed value: the auth package owns credentials, this
// package owns *who someone is*. Keeping the split means a future OAuth or
// passkey flow (docs/10 §34) can create users without going through password
// registration.
package users

import (
	"time"

	"github.com/google/uuid"
)

// Role is a platform-wide role (docs/08 §6.1, docs/10 §19).
//
// "Writer" is intentionally absent. docs/10 §52 models writing as a capability
// a normal user gains by creating a fiction, not a separate account type - so a
// reader who publishes something does not change role.
type Role string

const (
	RoleUser      Role = "user"
	RoleModerator Role = "moderator"
	RoleAdmin     Role = "admin"
)

func (r Role) Valid() bool {
	switch r {
	case RoleUser, RoleModerator, RoleAdmin:
		return true
	}
	return false
}

// IsStaff reports whether the role carries moderation or administrative
// permissions. Ownership is still checked separately (docs/10 §19).
func (r Role) IsStaff() bool { return r == RoleModerator || r == RoleAdmin }

// Status is the account lifecycle state (docs/10 §18).
type Status string

const (
	StatusActive              Status = "active"
	StatusPendingVerification Status = "pending_verification"
	StatusSuspended           Status = "suspended"
	StatusBanned              Status = "banned"
	StatusDeleted             Status = "deleted"
)

func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusPendingVerification, StatusSuspended, StatusBanned, StatusDeleted:
		return true
	}
	return false
}

// CanAuthenticate reports whether an account in this state may hold a session.
//
// PENDING_VERIFICATION is allowed: docs/10 §17 says not to block basic usage on
// email verification. Verification gates publishing, not signing in.
func (s Status) CanAuthenticate() bool {
	return s == StatusActive || s == StatusPendingVerification
}

// User is the identity record. It mirrors the `users` table.
//
// PasswordHash is unexported-by-convention through the DTO layer: no API
// response ever includes it (docs/10 §9), which the Public() method enforces.
type User struct {
	ID              uuid.UUID
	Username        string
	Email           string
	PasswordHash    string
	Role            Role
	Status          Status
	EmailVerifiedAt *time.Time
	// AdultAttestedAt is when the account stated it belongs to an adult (§13B).
	// A timestamp rather than a boolean, because "when" is the only part of the
	// answer worth keeping - there is deliberately no date of birth and no
	// document behind it (docs/11 §34).
	AdultAttestedAt           *time.Time
	SessionsInvalidatedBefore *time.Time
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	DeletedAt                 *time.Time
}

// EmailVerified reports whether the address has been confirmed.
//
// docs/10 §17 and the Phase 1 decision: verification is NOT required to read or
// to hold an account, but IS required before publishing writer content.
func (u *User) EmailVerified() bool { return u.EmailVerifiedAt != nil }

// AdultAttested reports whether the account has stated it belongs to an adult.
// Required once before publishing 18+ work, and never asked again (§13B).
func (u *User) AdultAttested() bool { return u != nil && u.AdultAttestedAt != nil }

// Active reports whether the account may sign in.
func (u *User) Active() bool { return u.DeletedAt == nil && u.Status.CanAuthenticate() }

// PublicUser is the shape returned to clients.
//
// This type is the reason a password hash cannot leak by accident: handlers
// return PublicUser, never User, so a new field on User is not silently
// published (docs/08 §1.4, docs/09 §13).
type PublicUser struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name,omitempty"`
	AvatarURL   *string   `json:"avatar_url,omitempty"`
	Bio         *string   `json:"bio,omitempty"`
}

// PrivateUser is what the authenticated user sees about *themselves*.
//
// It adds fields a user may see on their own account but which must never
// appear on another user's public profile - the email address above all
// (docs/10 §8).
type PrivateUser struct {
	PublicUser
	Email         string `json:"email"`
	Role          Role   `json:"role"`
	Status        Status `json:"status"`
	EmailVerified bool   `json:"email_verified"`
	// AdultAttested is sent so the studio can show the 18+ ratings as reachable
	// or not before the writer picks one, rather than after the publish fails.
	AdultAttested bool      `json:"adult_attested"`
	CreatedAt     time.Time `json:"created_at"`
	// True once the user owns an author profile; clients use it to decide
	// whether to surface Writer Studio.
	IsAuthor bool `json:"is_author"`
}

// Profile is the public profile record (docs/08 §6.2).
type Profile struct {
	UserID      uuid.UUID
	DisplayName *string
	Bio         *string
	AvatarURL   *string
	WebsiteURL  *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Preferences are reader settings (docs/08 §6.4).
type Preferences struct {
	UserID       uuid.UUID `json:"-"`
	Theme        string    `json:"theme"`
	FontSize     string    `json:"font_size"`
	ReadingWidth string    `json:"reading_width"`
	LineSpacing  string    `json:"line_spacing"`
	Language     string    `json:"language"`
}

// Public renders the user as a public profile.
func (u *User) Public(profile *Profile) PublicUser {
	out := PublicUser{ID: u.ID, Username: u.Username}
	if profile != nil {
		out.DisplayName = profile.DisplayName
		out.AvatarURL = profile.AvatarURL
		out.Bio = profile.Bio
	}
	return out
}

// Private renders the user's own account view.
func (u *User) Private(profile *Profile, isAuthor bool) PrivateUser {
	return PrivateUser{
		PublicUser:    u.Public(profile),
		Email:         u.Email,
		Role:          u.Role,
		Status:        u.Status,
		EmailVerified: u.EmailVerified(),
		AdultAttested: u.AdultAttested(),
		CreatedAt:     u.CreatedAt,
		IsAuthor:      isAuthor,
	}
}

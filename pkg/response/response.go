// Package response renders the standard FictionThai API envelope.
//
// Contract (docs/09 - API Specification.md §7):
//
//	single:     {"data": {...}}
//	collection: {"data": [...], "meta": {"page":1,"per_page":20,"total":0}}
//	error:      {"error": {"code":"...","message":"...","fields":{...}}}
//
// Handlers should always go through this package so every client sees one shape.
package response

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

type envelope struct {
	Data any              `json:"data"`
	Meta *pagination.Meta `json:"meta,omitempty"`
}

type errorEnvelope struct {
	Error *apierror.Error `json:"error"`
}

// Data writes a single-resource response.
func Data(c *gin.Context, status int, data any) {
	c.JSON(status, envelope{Data: data})
}

// OK writes a 200 single-resource response.
func OK(c *gin.Context, data any) { Data(c, http.StatusOK, data) }

// Created writes a 201 single-resource response.
func Created(c *gin.Context, data any) { Data(c, http.StatusCreated, data) }

// NoContent writes a 204 with no body.
func NoContent(c *gin.Context) { c.Status(http.StatusNoContent) }

// Collection writes a paginated collection response. A nil or empty slice is
// still rendered as `[]`, never `null`, so clients can iterate unconditionally.
func Collection(c *gin.Context, data any, meta pagination.Meta) {
	if data == nil {
		data = []any{}
	}
	c.JSON(http.StatusOK, envelope{Data: data, Meta: &meta})
}

// Fail renders err using the API error contract. Any error that is not an
// *apierror.Error is treated as an internal error, so unexpected failures can
// never leak internal detail to a client.
func Fail(c *gin.Context, err error) {
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) {
		apiErr = apierror.Internal()
	}
	c.AbortWithStatusJSON(apiErr.Status, errorEnvelope{Error: apiErr})
}

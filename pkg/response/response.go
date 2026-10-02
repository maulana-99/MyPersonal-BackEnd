package response

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// envelope is the single response shape. `Data` is NOT omitempty: an endpoint
// that legitimately has nothing to report (e.g. "no active focus session") must
// be able to say `null` rather than omit the key, so the client can tell
// "empty" apart from "the server sent something unexpected".
type envelope struct {
	Data    any    `json:"data"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, envelope{Data: data})
}

func Created(c *gin.Context, data any) {
	c.JSON(http.StatusCreated, envelope{Data: data})
}

func NoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func BadRequest(c *gin.Context, msg string) {
	c.JSON(http.StatusBadRequest, envelope{Error: msg})
}

func Unauthorized(c *gin.Context, msg string) {
	c.JSON(http.StatusUnauthorized, envelope{Error: msg})
}

func Forbidden(c *gin.Context) {
	c.JSON(http.StatusForbidden, envelope{Error: "forbidden"})
}

func NotFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, envelope{Error: "not found"})
}

// Conflict reports a state clash (409) — e.g. a second live focus session.
// Distinct from respondDBError's unique-violation branch, which is a data clash.
func Conflict(c *gin.Context, msg string) {
	c.JSON(http.StatusConflict, envelope{Error: msg})
}

func Internal(c *gin.Context, err error) {
	// ponytail: no sentry/logging here; add when observability added
	c.JSON(http.StatusInternalServerError, envelope{Error: "internal server error"})
}

package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres error codes we translate into HTTP statuses.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
	pgNotNullViolation    = "23502"
)

// paramUUID reads a path parameter as a UUID. On failure it writes 400 and
// returns ok=false, so callers can `if !ok { return }`.
func paramUUID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		response.BadRequest(c, "invalid "+name+": must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

// respondDBError maps a database error onto the right HTTP status.
// Callers stay free of pgx/pgconn imports.
func respondDBError(c *gin.Context, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		response.NotFound(c)
		return
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			c.JSON(http.StatusConflict, gin.H{"error": "resource already exists"})
			return
		case pgForeignKeyViolation:
			response.BadRequest(c, "referenced resource does not exist")
			return
		case pgCheckViolation, pgNotNullViolation:
			response.BadRequest(c, "invalid field value")
			return
		}
	}

	response.Internal(c, err)
}

// page holds normalised pagination. Defaults keep a naive client from
// pulling an unbounded table.
type page struct {
	Limit  int
	Offset int
}

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

func pagination(c *gin.Context) page {
	limit, err := strconv.Atoi(c.Query("limit"))
	if err != nil || limit < 1 {
		return page{Limit: defaultPageLimit}
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}

	offset, err := strconv.Atoi(c.Query("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}

	return page{Limit: limit, Offset: offset}
}

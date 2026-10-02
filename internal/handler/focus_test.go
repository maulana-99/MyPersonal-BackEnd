package handler

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func focusRouter() *gin.Engine {
	h := NewFocusHandler(testDB)
	category := NewCategoryHandler(testDB)
	return newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/categories", category.List)
		g.POST("/categories", category.Create)

		g.GET("/focus-sessions", h.List)
		g.POST("/focus-sessions", h.Start)
		g.GET("/focus-sessions/active", h.Active)
		g.GET("/focus-sessions/stats", h.Stats)
		g.GET("/focus-sessions/:id", h.Get)
		g.DELETE("/focus-sessions/:id", h.Delete)
		g.POST("/focus-sessions/:id/pause", h.Pause)
		g.POST("/focus-sessions/:id/resume", h.Resume)
		g.POST("/focus-sessions/:id/complete", h.Complete)
		g.POST("/focus-sessions/:id/cancel", h.Cancel)
	})
}

func TestFocusSessionLifecycle(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := focusRouter()

	// No live session yet: active is an explicit null, not a 404.
	w := doJSON(t, r, http.MethodGet, "/focus-sessions/active", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Nil(t, decodeData[*FocusSession](t, w))

	w = doJSON(t, r, http.MethodPost, "/focus-sessions", token, gin.H{
		"title": "Deep Work", "goal": "Finish V2 backend", "planned_minutes": 50,
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	s := decodeData[FocusSession](t, w)
	assert.Equal(t, "running", s.Status)
	assert.Equal(t, 50, s.PlannedMinutes)
	require.Nil(t, s.EndedAt)
	assert.Equal(t, 0, s.ActualMinutes)

	id := s.ID.String()

	// The single-active-session rule: a second start is a conflict, not a
	// second timer.
	w = doJSON(t, r, http.MethodPost, "/focus-sessions", token, gin.H{
		"title": "Another", "planned_minutes": 25,
	})
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())

	// Active reflects the running session.
	w = doJSON(t, r, http.MethodGet, "/focus-sessions/active", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	active := decodeData[*FocusSession](t, w)
	require.NotNil(t, active)
	assert.Equal(t, s.ID, active.ID)

	// Pause then resume; a double pause is rejected.
	w = doJSON(t, r, http.MethodPost, "/focus-sessions/"+id+"/pause", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "paused", decodeData[FocusSession](t, w).Status)

	w = doJSON(t, r, http.MethodPost, "/focus-sessions/"+id+"/pause", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/focus-sessions/"+id+"/resume", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "running", decodeData[FocusSession](t, w).Status)

	// Complete: ended_at set, actual_minutes derived from the stored clock.
	w = doJSON(t, r, http.MethodPost, "/focus-sessions/"+id+"/complete", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	done := decodeData[FocusSession](t, w)
	assert.Equal(t, "completed", done.Status)
	require.NotNil(t, done.EndedAt)
	assert.GreaterOrEqual(t, done.ActualMinutes, 0)
	assert.LessOrEqual(t, done.ActualMinutes, 2, "the session ran for seconds, not minutes")

	// A completed session frees the slot.
	w = doJSON(t, r, http.MethodPost, "/focus-sessions", token, gin.H{
		"title": "Second block", "planned_minutes": 25,
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	second := decodeData[FocusSession](t, w)

	// Cancel discards it (and it must not count towards statistics).
	w = doJSON(t, r, http.MethodPost, "/focus-sessions/"+second.ID.String()+"/cancel", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "cancelled", decodeData[FocusSession](t, w).Status)

	// List shows both; the completed one is what statistics counts.
	w = doJSON(t, r, http.MethodGet, "/focus-sessions", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := decodeData[[]FocusSession](t, w)
	assert.Len(t, list, 2)

	w = doJSON(t, r, http.MethodGet, "/focus-sessions/stats", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Delete only a closed session.
	w = doJSON(t, r, http.MethodDelete, "/focus-sessions/"+id, token, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
}

func TestFocusSessionOwnershipAndValidation(t *testing.T) {
	requireDB(t)
	_, tokenA := newUser(t)
	_, tokenB := newUser(t)
	r := focusRouter()

	w := doJSON(t, r, http.MethodPost, "/focus-sessions", tokenA, gin.H{
		"title": "Mine", "planned_minutes": 25,
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	id := decodeData[FocusSession](t, w).ID.String()

	w = doJSON(t, r, http.MethodPost, "/focus-sessions/"+id+"/pause", tokenB, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/focus-sessions/"+id, tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	// Validation: title and planned_minutes are required and bounded.
	w = doJSON(t, r, http.MethodPost, "/focus-sessions", tokenB, gin.H{"planned_minutes": 25})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/focus-sessions", tokenB, gin.H{"title": "X", "planned_minutes": 0})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/focus-sessions", tokenB, gin.H{"title": "X", "planned_minutes": 9999})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestFocusSessionForeignKeyMustBeOwned(t *testing.T) {
	requireDB(t)
	_, tokenA := newUser(t)
	_, tokenB := newUser(t)
	r := focusRouter()

	// A category owned by user A cannot be attached by user B.
	w := doJSON(t, r, http.MethodPost, "/categories", tokenA, gin.H{"name": "A work"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	catID := decodeData[Category](t, w).ID

	w = doJSON(t, r, http.MethodPost, "/focus-sessions", tokenB, gin.H{
		"title": "Borrowed category", "planned_minutes": 25, "category_id": catID,
	})
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

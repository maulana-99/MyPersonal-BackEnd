package handler

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func routineRouter() *gin.Engine {
	h := NewRoutineHandler(testDB)
	return newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/routines", h.List)
		g.POST("/routines", h.Create)
		g.GET("/routines/:id", h.Get)
		g.PUT("/routines/:id", h.Update)
		g.DELETE("/routines/:id", h.Delete)
		g.PUT("/routines/:id/items/order", h.Reorder)
		g.POST("/routines/:id/items/:itemId/toggle", h.ToggleItem)
	})
}

func TestRoutineStartTimeAndToggle(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := routineRouter()

	start := "2026-09-28T07:00:00Z"

	w := doJSON(t, r, http.MethodPost, "/routines", token, gin.H{
		"name": "Morning", "start_time": start,
		"items": []gin.H{{"title": "Stretch"}, {"title": "Shower"}},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	routine := decodeData[Routine](t, w)
	require.NotNil(t, routine.StartTime)
	require.Len(t, routine.Items, 2)
	assert.False(t, routine.Items[0].CompletedToday)

	itemID := routine.Items[0].ID.String()

	// Toggle on → completed_today true.
	w = doJSON(t, r, http.MethodPost, "/routines/"+routine.ID.String()+"/items/"+itemID+"/toggle", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	toggled := decodeData[Routine](t, w)
	assert.True(t, toggled.Items[0].CompletedToday)

	// Toggle off → false again.
	w = doJSON(t, r, http.MethodPost, "/routines/"+routine.ID.String()+"/items/"+itemID+"/toggle", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	toggled = decodeData[Routine](t, w)
	assert.False(t, toggled.Items[0].CompletedToday)

	// Toggle a foreign item → 404.
	_, tokenB := newUser(t)
	w = doJSON(t, r, http.MethodPost, "/routines/"+routine.ID.String()+"/items/"+itemID+"/toggle", tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestRoutineCRUDAndReorder(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := routineRouter()

	w := doJSON(t, r, http.MethodPost, "/routines", token, gin.H{
		"name":        "Morning",
		"repeat_days": []string{"mon", "wed", "fri"},
		"items": []gin.H{
			{"title": "Stretch", "duration": 5},
			{"title": "Shower", "duration": 10},
			{"title": "Coffee", "duration": 5},
		},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	routine := decodeData[Routine](t, w)
	assert.Equal(t, "Morning", routine.Name)
	assert.Equal(t, []string{"mon", "wed", "fri"}, routine.RepeatDays)
	require.Len(t, routine.Items, 3)
	assert.Equal(t, "Stretch", routine.Items[0].Title)
	assert.Equal(t, 0, routine.Items[0].SortOrder)

	// Reorder: put Coffee first.
	ids := []string{
		routine.Items[2].ID.String(),
		routine.Items[0].ID.String(),
		routine.Items[1].ID.String(),
	}
	w = doJSON(t, r, http.MethodPut, "/routines/"+routine.ID.String()+"/items/order", token, gin.H{"item_ids": ids})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	reordered := decodeData[Routine](t, w)
	require.Len(t, reordered.Items, 3)
	assert.Equal(t, "Coffee", reordered.Items[0].Title)
	assert.Equal(t, 0, reordered.Items[0].SortOrder)
	assert.Equal(t, "Stretch", reordered.Items[1].Title)
	assert.Equal(t, "Shower", reordered.Items[2].Title)

	// Update replaces the item list.
	w = doJSON(t, r, http.MethodPut, "/routines/"+routine.ID.String(), token, gin.H{
		"name":        "Morning v2",
		"repeat_days": []string{"tue"},
		"items":       []gin.H{{"title": "Only step"}},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	updated := decodeData[Routine](t, w)
	assert.Equal(t, "Morning v2", updated.Name)
	require.Len(t, updated.Items, 1)
	assert.Equal(t, "Only step", updated.Items[0].Title)

	// Delete
	w = doJSON(t, r, http.MethodDelete, "/routines/"+routine.ID.String(), token, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
}

func TestRoutineOwnershipAndReorderValidation(t *testing.T) {
	requireDB(t)
	_, tokenA := newUser(t)
	_, tokenB := newUser(t)
	r := routineRouter()

	w := doJSON(t, r, http.MethodPost, "/routines", tokenA, gin.H{
		"name":  "Private",
		"items": []gin.H{{"title": "step"}},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	routine := decodeData[Routine](t, w)

	w = doJSON(t, r, http.MethodGet, "/routines/"+routine.ID.String(), tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPut, "/routines/"+routine.ID.String()+"/items/order", tokenB, gin.H{"item_ids": []string{routine.Items[0].ID.String()}})
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	// Item from another routine → 400.
	w = doJSON(t, r, http.MethodPut, "/routines/"+routine.ID.String()+"/items/order", tokenA, gin.H{"item_ids": []string{"00000000-0000-0000-0000-000000000001"}})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestRoutineValidation(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := routineRouter()

	w := doJSON(t, r, http.MethodPost, "/routines", token, gin.H{
		"name":        "Bad days",
		"repeat_days": []string{"monday"},
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

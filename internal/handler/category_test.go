package handler

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func categoryRouter() *gin.Engine {
	h := NewCategoryHandler(testDB)
	return newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/categories", h.List)
		g.POST("/categories", h.Create)
		g.PUT("/categories/:id", h.Update)
		g.DELETE("/categories/:id", h.Delete)
	})
}

func TestCategoryCRUD(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := categoryRouter()

	// Create
	w := doJSON(t, r, http.MethodPost, "/categories", token, gin.H{
		"name": "Writing", "color": "#123456",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	created := decodeData[Category](t, w)
	require.NotEqual(t, [16]byte{}, created.ID)
	assert.Equal(t, "Writing", created.Name)
	assert.Equal(t, "#123456", created.Color)

	// List
	w = doJSON(t, r, http.MethodGet, "/categories", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := decodeData[[]Category](t, w)
	require.Len(t, list, 1)
	assert.Equal(t, created.ID, list[0].ID)

	// Update
	w = doJSON(t, r, http.MethodPut, "/categories/"+created.ID.String(), token, gin.H{
		"name": "Writing 2", "color": "#654321",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	updated := decodeData[Category](t, w)
	assert.Equal(t, "Writing 2", updated.Name)
	assert.Equal(t, "#654321", updated.Color)

	// Delete
	w = doJSON(t, r, http.MethodDelete, "/categories/"+created.ID.String(), token, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

	// Gone → 404
	w = doJSON(t, r, http.MethodDelete, "/categories/"+created.ID.String(), token, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestCategoryOwnershipNotFound(t *testing.T) {
	requireDB(t)
	_, tokenA := newUser(t)
	_, tokenB := newUser(t)
	r := categoryRouter()

	w := doJSON(t, r, http.MethodPost, "/categories", tokenA, gin.H{"name": "Private"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	cat := decodeData[Category](t, w)

	// B cannot see, update, or delete A's row.
	w = doJSON(t, r, http.MethodGet, "/categories", tokenB, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, decodeData[[]Category](t, w))

	w = doJSON(t, r, http.MethodPut, "/categories/"+cat.ID.String(), tokenB, gin.H{"name": "Stolen"})
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodDelete, "/categories/"+cat.ID.String(), tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestCategoryDuplicateConflict(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := categoryRouter()

	w := doJSON(t, r, http.MethodPost, "/categories", token, gin.H{"name": "Dup"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	// Same name, different case → unique index on lower(name) → 409.
	w = doJSON(t, r, http.MethodPost, "/categories", token, gin.H{"name": "dup"})
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
}

func TestCategoryInvalidUUID(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := categoryRouter()

	w := doJSON(t, r, http.MethodDelete, "/categories/not-a-uuid", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestDefaultCategoriesSeeded(t *testing.T) {
	require.Equal(t, 8, len(defaultCategories))
}

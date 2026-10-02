package handler

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Register must seed the 8 default categories (Plan.md §2.8).
func TestRegisterSeedsDefaultCategories(t *testing.T) {
	requireDB(t)

	authHandler := NewAuthHandler(testDB, testJWT)
	r := gin.New()
	r.POST("/auth/register", authHandler.Register)

	email := "seeded-" + t.Name() + "@example.com"
	w := doJSON(t, r, http.MethodPost, "/auth/register", "", gin.H{
		"name": "Seed User", "email": email, "password": "password123",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var userID string
	err := testDB.QueryRow(testCtx, `SELECT id FROM users WHERE email = $1`, email).Scan(&userID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(testCtx, `DELETE FROM users WHERE email = $1`, email)
	})

	var count int
	err = testDB.QueryRow(testCtx, `SELECT COUNT(*) FROM categories WHERE user_id = $1`, userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, len(defaultCategories), count, "register should seed every default category")

	// Duplicate email → 400, and no extra rows leak.
	w = doJSON(t, r, http.MethodPost, "/auth/register", "", gin.H{
		"name": "Seed User", "email": email, "password": "password123",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

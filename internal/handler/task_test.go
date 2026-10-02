package handler

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func taskRouter() *gin.Engine {
	h := NewTaskHandler(testDB)
	return newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/tasks", h.List)
		g.POST("/tasks", h.Create)
		g.GET("/tasks/:id", h.Get)
		g.PUT("/tasks/:id", h.Update)
		g.DELETE("/tasks/:id", h.Delete)
		g.POST("/tasks/:id/complete", h.Complete)
		g.POST("/tasks/:id/subtasks", h.AddSubtask)
		g.PUT("/tasks/:id/subtasks/:sid", h.UpdateSubtask)
		g.DELETE("/tasks/:id/subtasks/:sid", h.DeleteSubtask)
	})
}

func TestTaskCRUDAndSubtasks(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := taskRouter()

	w := doJSON(t, r, http.MethodPost, "/tasks", token, gin.H{
		"title": "Write report", "priority": "high", "due_date": "2026-10-01",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	task := decodeData[Task](t, w)
	assert.Equal(t, "high", task.Priority)
	assert.Equal(t, "pending", task.Status)
	require.NotNil(t, task.DueDate)
	assert.Equal(t, "2026-10-01", *task.DueDate)

	// Subtask add
	w = doJSON(t, r, http.MethodPost, "/tasks/"+task.ID.String()+"/subtasks", token, gin.H{"title": "Outline"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	sub := decodeData[Subtask](t, w)
	assert.False(t, sub.Completed)

	// Subtask complete
	w = doJSON(t, r, http.MethodPut, "/tasks/"+task.ID.String()+"/subtasks/"+sub.ID.String(), token, gin.H{"completed": true})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, decodeData[Subtask](t, w).Completed)

	// Task carries subtasks
	w = doJSON(t, r, http.MethodGet, "/tasks/"+task.ID.String(), token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	fetched := decodeData[Task](t, w)
	require.Len(t, fetched.Subtasks, 1)
	assert.True(t, fetched.Subtasks[0].Completed)

	// Toggle complete
	w = doJSON(t, r, http.MethodPost, "/tasks/"+task.ID.String()+"/complete", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "completed", decodeData[Task](t, w).Status)
	w = doJSON(t, r, http.MethodPost, "/tasks/"+task.ID.String()+"/complete", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "pending", decodeData[Task](t, w).Status)

	// Update keeps priority when omitted.
	w = doJSON(t, r, http.MethodPut, "/tasks/"+task.ID.String(), token, gin.H{"title": "Write report v2"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	updated := decodeData[Task](t, w)
	assert.Equal(t, "Write report v2", updated.Title)
	assert.Equal(t, "high", updated.Priority)

	// Delete subtask then task
	w = doJSON(t, r, http.MethodDelete, "/tasks/"+task.ID.String()+"/subtasks/"+sub.ID.String(), token, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodDelete, "/tasks/"+task.ID.String(), token, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
}

func TestTaskListFilters(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := taskRouter()

	for _, spec := range []struct{ title, prio string }{{"A", "low"}, {"B", "high"}} {
		w := doJSON(t, r, http.MethodPost, "/tasks", token, gin.H{"title": spec.title, "priority": spec.prio})
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	}

	w := doJSON(t, r, http.MethodGet, "/tasks?status=pending", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Len(t, decodeData[[]Task](t, w), 2)

	w = doJSON(t, r, http.MethodGet, "/tasks?status=bogus", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestTaskOwnership(t *testing.T) {
	requireDB(t)
	_, tokenA := newUser(t)
	_, tokenB := newUser(t)
	r := taskRouter()

	w := doJSON(t, r, http.MethodPost, "/tasks", tokenA, gin.H{"title": "Mine"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	task := decodeData[Task](t, w)

	w = doJSON(t, r, http.MethodGet, "/tasks/"+task.ID.String(), tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/tasks/"+task.ID.String()+"/subtasks", tokenB, gin.H{"title": "x"})
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodDelete, "/tasks/"+task.ID.String(), tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestTaskValidation(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := taskRouter()

	w := doJSON(t, r, http.MethodPost, "/tasks", token, gin.H{"title": "Bad prio", "priority": "urgent"})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/tasks", token, gin.H{"title": "Bad date", "due_date": "01-10-2026"})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

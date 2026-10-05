package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// main() builds its router inline, so handler tests cannot see a forgotten
// registration. This guards the wiring calls in the real entry point.
func TestMainWiresNotes(t *testing.T) {
	src, err := os.ReadFile("main.go")
	assert.NoError(t, err)
	for _, want := range []string{"handler.RegisterNoteRoutes(protected, noteHandler)", "noteHandler.SetDrive(", "googlecal.NewDrive("} {
		assert.Contains(t, string(src), want)
	}
}

// The Drive routes (browse, upload, edit, trash, delete-from-trash) must all go
// through RegisterDriveRoutes, whose exact route list is tested in the handler
// package: main.go may not register any /drive route of its own.
func TestMainWiresDrive(t *testing.T) {
	src, err := os.ReadFile("main.go")
	assert.NoError(t, err)
	for _, want := range []string{"handler.RegisterDriveRoutes(protected, driveHandler)",
		"handler.NewDriveHandler(googlecal.NewFilesService("} {
		assert.Contains(t, string(src), want)
	}
	for _, bad := range []string{`protected.POST("/drive`, `protected.PUT("/drive`, `protected.PATCH("/drive`, `protected.DELETE("/drive`} {
		assert.NotContains(t, string(src), bad)
	}
}

// Tasks live in Google Tasks for connected users: main.go must wire the
// service into both the task and today handlers and register routes centrally.
func TestMainWiresTasks(t *testing.T) {
	src, err := os.ReadFile("main.go")
	assert.NoError(t, err)
	for _, want := range []string{"handler.RegisterTaskRoutes(protected, taskHandler)", "taskHandler.SetGoogle(tasksSvc)",
		"todayHandler.SetGoogle(tasksSvc)", "googlecal.NewTasks("} {
		assert.Contains(t, string(src), want)
	}
	assert.NotContains(t, string(src), `protected.GET("/tasks`)
}

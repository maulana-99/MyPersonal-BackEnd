package handler

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
)

// GoogleHandler serves /integrations/google/*. svc is nil when the feature is
// not configured.
type GoogleHandler struct {
	svc         *googlecal.Service
	frontendURL string
}

func NewGoogleHandler(svc *googlecal.Service, frontendURL string) *GoogleHandler {
	return &GoogleHandler{svc: svc, frontendURL: frontendURL}
}

// googleError maps service errors onto the contract's status + code pairs.
func googleError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, googlecal.ErrNotConnected):
		response.Error(c, http.StatusConflict, "not_connected")
	case errors.Is(err, googlecal.ErrNeedsReauth):
		response.Error(c, http.StatusConflict, "needs_reauth")
	case errors.Is(err, googlecal.ErrAPI):
		response.Error(c, http.StatusBadGateway, "google_api_error")
	default:
		response.Internal(c, err)
	}
}

// configured writes 503 and returns false when the feature is off.
func (h *GoogleHandler) configured(c *gin.Context) bool {
	if h.svc == nil {
		response.Error(c, http.StatusServiceUnavailable, "google_not_configured")
		return false
	}
	return true
}

func (h *GoogleHandler) Connect(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	u, err := h.svc.ConnectURL(middleware.GetUserID(c))
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"url": u})
}

// Callback is a browser redirect, so it never returns JSON: every outcome is a
// 302 to the frontend with a fixed reason code.
func (h *GoogleHandler) Callback(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	reason := googlecal.ReasonDenied
	if c.Query("error") == "" {
		reason = h.svc.HandleCallback(c, c.Query("code"), c.Query("state"))
	}
	target := h.frontendURL + "/calendar?google=connected"
	if reason != "" {
		target = h.frontendURL + "/calendar?google=error&reason=" + url.QueryEscape(reason)
	}
	c.Redirect(http.StatusFound, target)
}

func (h *GoogleHandler) Status(c *gin.Context) {
	if h.svc == nil {
		response.OK(c, googlecal.Status{Configured: false})
		return
	}
	st, err := h.svc.Status(c, middleware.GetUserID(c))
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, st)
}

func (h *GoogleHandler) Sync(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	st, err := h.svc.Sync(c, middleware.GetUserID(c))
	if err != nil {
		googleError(c, err)
		return
	}
	response.OK(c, st)
}

func (h *GoogleHandler) Disconnect(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	if err := h.svc.Disconnect(c, middleware.GetUserID(c)); err != nil {
		response.Internal(c, err)
		return
	}
	response.NoContent(c)
}

// UploadLocal pushes local-only schedules to Google.
func (h *GoogleHandler) UploadLocal(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	st, err := h.svc.UploadLocal(c, middleware.GetUserID(c), validRecurrence)
	if err != nil {
		googleError(c, err)
		return
	}
	response.OK(c, st)
}

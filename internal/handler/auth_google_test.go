package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	pkgjwt "github.com/chronaxis/daily-planner-backend/pkg/jwt"
	"github.com/chronaxis/daily-planner-backend/pkg/password"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type loginEnv struct {
	f *fakeGoogle
	r *gin.Engine
}

func newLoginEnv(t *testing.T, configured, allowPassword bool, allowed ...string) loginEnv {
	t.Helper()
	f := &fakeGoogle{}
	ah := NewAuthHandler(testDB, testJWT)
	if configured {
		svc := googlecal.NewService(testDB, f, testKey, stateSecret)
		svc.Go = func(fn func()) { fn() }
		ah.SetGoogle(svc, "http://front", allowed, allowPassword)
	}
	r := gin.New()
	r.GET("/auth/google/login", ah.GoogleLogin)
	r.GET("/auth/google/callback", ah.GoogleCallback)
	r.POST("/auth/register", ah.Register)
	r.POST("/auth/login", ah.Login)
	return loginEnv{f, r}
}

// newIdentity registers an identity on the fake and removes the user it may create.
func newIdentity(t *testing.T, e loginEnv) googlecal.Identity {
	t.Helper()
	email := "g-" + uuid.NewString() + "@example.com"
	id := googlecal.Identity{Sub: "sub-" + uuid.NewString(), Email: email, Name: "Gina Google",
		EmailVerified: true, RefreshToken: "rt-login"}
	e.f.loginID = id
	t.Cleanup(func() { _, _ = testDB.Exec(testCtx, `DELETE FROM users WHERE email = $1`, email) })
	return id
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// startLogin hits /auth/google/login and returns the state and nonce cookie.
func startLogin(t *testing.T, e loginEnv) (state string, nonce *http.Cookie) {
	t.Helper()
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/google/login", nil))
	require.Equal(t, http.StatusFound, w.Code)
	u, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	state = u.Query().Get("state")
	require.NotEmpty(t, state)
	nonce = cookieNamed(w, "g_nonce")
	require.NotNil(t, nonce)
	assert.True(t, nonce.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, nonce.SameSite)
	assert.Equal(t, 600, nonce.MaxAge)
	return state, nonce
}

func callback(e loginEnv, query string, nonce *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/auth/google/callback?"+query, nil)
	if nonce != nil {
		req.AddCookie(nonce)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func TestGoogleLoginNotConfigured(t *testing.T) {
	requireDB(t)
	e := newLoginEnv(t, false, false)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/google/login", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "google_not_configured")
}

func TestGoogleLoginCreatesUser(t *testing.T) {
	requireDB(t)
	e := newLoginEnv(t, true, false)
	id := newIdentity(t, e)

	state, nonce := startLogin(t, e)
	w := callback(e, "code=c&state="+url.QueryEscape(state), nonce)
	require.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "http://front/", w.Header().Get("Location"))

	// same refresh cookie as password login, and it verifies
	rc := cookieNamed(w, "refresh_token")
	require.NotNil(t, rc)
	assert.True(t, rc.HttpOnly)
	assert.True(t, rc.Secure)
	assert.Equal(t, 7*24*3600, rc.MaxAge)
	var uid uuid.UUID
	var hash *string
	var cats int
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT id, password_hash FROM users WHERE google_sub = $1`, id.Sub).Scan(&uid, &hash))
	assert.Nil(t, hash)
	claims, err := testJWT.Verify(rc.Value)
	require.NoError(t, err)
	assert.Equal(t, pkgjwt.RefreshToken, claims.TokenType)
	assert.Equal(t, uid, claims.UserID)
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT COUNT(*) FROM categories WHERE user_id = $1`, uid).Scan(&cats))
	assert.Equal(t, len(defaultCategories), cats)

	// nonce cleared, integration stored encrypted, first sync ran
	cleared := cookieNamed(w, "g_nonce")
	require.NotNil(t, cleared)
	assert.Less(t, cleared.MaxAge, 0)
	var enc []byte
	var email string
	var synced bool
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT refresh_token_enc, google_email, last_synced_at IS NOT NULL
		FROM integrations_google WHERE user_id = $1`, uid).Scan(&enc, &email, &synced))
	assert.NotContains(t, string(enc), "rt-login")
	assert.Equal(t, id.Email, email)
	assert.True(t, synced)

	// logging in again finds the same user by sub (no duplicate)
	state, nonce = startLogin(t, e)
	w = callback(e, "code=c&state="+url.QueryEscape(state), nonce)
	assert.Equal(t, "http://front/", w.Header().Get("Location"))
	var n int
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT COUNT(*) FROM users WHERE email = $1`, id.Email).Scan(&n))
	assert.Equal(t, 1, n)

	// Google returns no refresh token: the stored one is kept
	e.f.loginID.RefreshToken = ""
	state, nonce = startLogin(t, e)
	w = callback(e, "code=c&state="+url.QueryEscape(state), nonce)
	assert.Equal(t, "http://front/", w.Header().Get("Location"))
}

func TestGoogleLoginLinksExistingUserByEmail(t *testing.T) {
	requireDB(t)
	e := newLoginEnv(t, true, false)
	uid, _ := newUser(t)
	var email string
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT email FROM users WHERE id = $1`, uid).Scan(&email))
	e.f.loginID = googlecal.Identity{Sub: "sub-" + uuid.NewString(), Email: email, EmailVerified: true, RefreshToken: "rt"}

	state, nonce := startLogin(t, e)
	w := callback(e, "code=c&state="+url.QueryEscape(state), nonce)
	assert.Equal(t, "http://front/", w.Header().Get("Location"))

	var sub, hash *string
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT google_sub, password_hash FROM users WHERE id = $1`, uid).Scan(&sub, &hash))
	require.NotNil(t, sub)
	assert.Equal(t, e.f.loginID.Sub, *sub)
	require.NotNil(t, hash, "existing account data is kept")
	rc := cookieNamed(w, "refresh_token")
	require.NotNil(t, rc)
	claims, err := testJWT.Verify(rc.Value)
	require.NoError(t, err)
	assert.Equal(t, uid, claims.UserID)
}

func TestGoogleLoginRejections(t *testing.T) {
	requireDB(t)
	e := newLoginEnv(t, true, false)
	id := newIdentity(t, e)
	e.f.loginID.RefreshToken = "" // no stored token either

	state, nonce := startLogin(t, e)
	q := "code=c&state=" + url.QueryEscape(state)
	loc := func(w *httptest.ResponseRecorder) string { return w.Header().Get("Location") }
	noSession := func(w *httptest.ResponseRecorder) {
		assert.Nil(t, cookieNamed(w, "refresh_token"))
	}

	// bad state, missing nonce cookie, wrong nonce cookie
	w := callback(e, "code=c&state=bogus", nonce)
	assert.Equal(t, "http://front/login?error=state", loc(w))
	noSession(w)
	w = callback(e, q, nil)
	assert.Equal(t, "http://front/login?error=state", loc(w))
	noSession(w)
	w = callback(e, q, &http.Cookie{Name: "g_nonce", Value: "other"})
	assert.Equal(t, "http://front/login?error=state", loc(w))
	// a connect-flow state is not a login state
	connect, err := googlecal.SignState(stateSecret, uuid.New(), googlecal.StateTTL)
	require.NoError(t, err)
	w = callback(e, "code=c&state="+connect, nonce)
	assert.Equal(t, "http://front/login?error=state", loc(w))

	// user denied consent
	w = callback(e, "error=access_denied&state="+url.QueryEscape(state), nonce)
	assert.Equal(t, "http://front/login?error=denied", loc(w))

	// exchange failure, unverified email
	e.f.loginErr = http.ErrAbortHandler
	w = callback(e, q, nonce)
	assert.Equal(t, "http://front/login?error=exchange", loc(w))
	e.f.loginErr = nil
	e.f.loginID.EmailVerified = false
	w = callback(e, q, nonce)
	assert.Equal(t, "http://front/login?error=exchange", loc(w))
	e.f.loginID.EmailVerified = true

	// no refresh token from Google and none stored
	w = callback(e, q, nonce)
	assert.Equal(t, "http://front/login?error=exchange", loc(w))
	noSession(w)

	var n int
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT COUNT(*) FROM integrations_google WHERE google_email = $1`, id.Email).Scan(&n))
	assert.Zero(t, n)
}

func TestGoogleLoginAllowlist(t *testing.T) {
	requireDB(t)
	email := "allowed-" + uuid.NewString() + "@example.com"
	e := newLoginEnv(t, true, false, "someone@else.com", email)
	e.f.loginID = googlecal.Identity{Sub: "s-" + uuid.NewString(), Email: "Nope-" + uuid.NewString() + "@example.com", EmailVerified: true, RefreshToken: "rt"}
	t.Cleanup(func() { _, _ = testDB.Exec(testCtx, `DELETE FROM users WHERE email = $1`, email) })

	state, nonce := startLogin(t, e)
	w := callback(e, "code=c&state="+url.QueryEscape(state), nonce)
	assert.Equal(t, "http://front/login?error=not_allowed", w.Header().Get("Location"))
	assert.Nil(t, cookieNamed(w, "refresh_token"))
	var n int
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT COUNT(*) FROM users WHERE google_sub = $1`, e.f.loginID.Sub).Scan(&n))
	assert.Zero(t, n)

	e.f.loginID.Email = email
	state, nonce = startLogin(t, e)
	w = callback(e, "code=c&state="+url.QueryEscape(state), nonce)
	assert.Equal(t, "http://front/", w.Header().Get("Location"))
}

func TestPasswordAuthSwitch(t *testing.T) {
	requireDB(t)
	email := "pw-" + uuid.NewString() + "@example.com"
	hash, err := password.Hash("password123")
	require.NoError(t, err)
	_, err = testDB.Exec(testCtx, `INSERT INTO users (name, email, password_hash) VALUES ('Pw', $1, $2)`, email, hash)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = testDB.Exec(testCtx, `DELETE FROM users WHERE email = $1`, email) })
	login := gin.H{"email": email, "password": "password123"}
	reg := gin.H{"name": "Pw Two", "email": "x-" + email, "password": "password123"}
	t.Cleanup(func() { _, _ = testDB.Exec(testCtx, `DELETE FROM users WHERE email = $1`, "x-"+email) })

	// Google configured: both disabled
	e := newLoginEnv(t, true, false)
	for path, body := range map[string]gin.H{"/auth/login": login, "/auth/register": reg} {
		w := doJSON(t, e.r, http.MethodPost, path, "", body)
		assert.Equal(t, http.StatusForbidden, w.Code, path)
		assert.Contains(t, w.Body.String(), "password_auth_disabled")
	}

	// ALLOW_PASSWORD_AUTH=true: works as before
	e = newLoginEnv(t, true, true)
	w := doJSON(t, e.r, http.MethodPost, "/auth/login", "", login)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotNil(t, cookieNamed(w, "refresh_token"))

	// Google not configured: as before
	e = newLoginEnv(t, false, false)
	w = doJSON(t, e.r, http.MethodPost, "/auth/login", "", login)
	assert.Equal(t, http.StatusOK, w.Code)

	// Google-created user (NULL hash) cannot log in with a password
	_, err = testDB.Exec(testCtx, `UPDATE users SET password_hash = NULL WHERE email = $1`, email)
	require.NoError(t, err)
	w = doJSON(t, e.r, http.MethodPost, "/auth/login", "", login)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid credentials")
}

func TestGoogleLoginStoresAvatarOnlyIfHTTPS(t *testing.T) {
	requireDB(t)
	e := newLoginEnv(t, true, false)
	id := newIdentity(t, e)

	avatar := func() *string {
		var a *string
		require.NoError(t, testDB.QueryRow(testCtx, `SELECT avatar_url FROM users WHERE google_sub = $1`, id.Sub).Scan(&a))
		return a
	}
	login := func() {
		state, nonce := startLogin(t, e)
		w := callback(e, "code=c&state="+url.QueryEscape(state), nonce)
		require.Equal(t, "http://front/", w.Header().Get("Location"))
	}

	e.f.loginID.Picture = "http://insecure.example/a.png"
	login()
	assert.Nil(t, avatar(), "non-https picture must not be stored")

	e.f.loginID.Picture = "https://lh3.googleusercontent.com/a/photo=s96-c"
	login()
	require.NotNil(t, avatar())
	assert.Equal(t, "https://lh3.googleusercontent.com/a/photo=s96-c", *avatar())

	// a later login without a picture keeps the stored one
	e.f.loginID.Picture = ""
	login()
	require.NotNil(t, avatar())
}

package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	pkgjwt "github.com/chronaxis/daily-planner-backend/pkg/jwt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mobVerifier = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ-._~123" // 50 chars

func mobChallenge(v string) string {
	s := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

type memOnce struct {
	seen map[string]bool
	err  error
}

func (m *memOnce) Once(_ context.Context, key string, _ time.Duration) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	if m.seen[key] {
		return false, nil
	}
	m.seen[key] = true
	return true, nil
}

type mobEnv struct {
	r    *gin.Engine
	ah   *AuthHandler
	svc  *googlecal.Service
	jwt  *pkgjwt.Manager
	once *memOnce
}

// newMobEnv needs no database: only the paths that never touch it are exercised.
func newMobEnv(t *testing.T) mobEnv {
	t.Helper()
	jm := pkgjwt.NewManager(strings.Repeat("mobile-secret-", 4), 15*time.Minute, time.Hour)
	svc := googlecal.NewService(nil, &fakeGoogle{}, testKey, stateSecret)
	ah := NewAuthHandler(nil, jm)
	ah.SetGoogle(svc, "http://front", nil, false)
	once := &memOnce{seen: map[string]bool{}}
	ah.SetMobile(once)
	r := gin.New()
	r.GET("/auth/google/login", ah.GoogleLogin)
	r.GET("/auth/google/callback", ah.GoogleCallback)
	r.POST("/auth/mobile/exchange", ah.MobileExchange)
	r.POST("/auth/refresh", ah.Refresh)
	return mobEnv{r, ah, svc, jm, once}
}

func (e mobEnv) do(method, path, body string, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func TestMobileLoginChallengeValidation(t *testing.T) {
	e := newMobEnv(t)
	for _, q := range []string{
		"client=mobile", "client=mobile&code_challenge=short",
		"client=mobile&code_challenge=" + strings.Repeat("a", 44),
		"client=mobile&code_challenge=" + strings.Repeat("+", 43),
	} {
		w := e.do(http.MethodGet, "/auth/google/login?"+q, "", nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, q)
		assert.Contains(t, w.Body.String(), "invalid_challenge")
	}
}

func startMob(t *testing.T, e mobEnv, query string) (state string, nonce *http.Cookie) {
	t.Helper()
	w := e.do(http.MethodGet, "/auth/google/login"+query, "", nil)
	require.Equal(t, http.StatusFound, w.Code)
	u, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	return u.Query().Get("state"), cookieNamed(w, "g_nonce")
}

func TestMobileStateRoundTrip(t *testing.T) {
	e := newMobEnv(t)
	ch := mobChallenge(mobVerifier)
	state, nonce := startMob(t, e, "?client=mobile&code_challenge="+ch)
	require.NotNil(t, nonce)
	got, mobile := e.svc.MobileChallenge(state)
	assert.True(t, mobile)
	assert.Equal(t, ch, got)

	// web login (no client, or an unknown client) never produces a mobile state
	for _, q := range []string{"", "?client=web&code_challenge=" + ch, "?code_challenge=" + ch} {
		state, _ := startMob(t, e, q)
		_, mobile := e.svc.MobileChallenge(state)
		assert.False(t, mobile, q)
	}
	_, mobile = e.svc.MobileChallenge("garbage")
	assert.False(t, mobile)
}

func TestMobileCallbackRedirectsOnlyToConstant(t *testing.T) {
	e := newMobEnv(t)
	state, nonce := startMob(t, e, "?client=mobile&code_challenge="+mobChallenge(mobVerifier))

	// denied by the user -> deep link with error, never FRONTEND_URL
	w := e.do(http.MethodGet, "/auth/google/callback?error=access_denied&state="+url.QueryEscape(state), "", nil, nonce)
	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "mypersonal://oauth?error=denied", w.Header().Get("Location"))

	// bad nonce with a valid mobile state
	w = e.do(http.MethodGet, "/auth/google/callback?code=c&state="+url.QueryEscape(state), "", nil)
	assert.Equal(t, "mypersonal://oauth?error=state", w.Header().Get("Location"))

	// unverifiable state cannot be trusted to be mobile: web error redirect
	w = e.do(http.MethodGet, "/auth/google/callback?code=c&state=forged", "", nil, nonce)
	assert.Equal(t, "http://front/login?error=state", w.Header().Get("Location"))
}

func TestMobileCallbackSuccessRedirect(t *testing.T) {
	requireDB(t)
	f := &fakeGoogle{}
	ah := NewAuthHandler(testDB, testJWT)
	svc := googlecal.NewService(testDB, f, testKey, stateSecret)
	svc.Go = func(fn func()) { fn() }
	ah.SetGoogle(svc, "http://front", nil, false)
	e := mobEnv{r: gin.New(), ah: ah, svc: svc}
	e.r.GET("/auth/google/login", ah.GoogleLogin)
	e.r.GET("/auth/google/callback", ah.GoogleCallback)
	email := "m-" + uuid.NewString() + "@example.com"
	f.loginID = googlecal.Identity{Sub: "sub-" + uuid.NewString(), Email: email, EmailVerified: true, RefreshToken: "rt"}
	t.Cleanup(func() { _, _ = testDB.Exec(testCtx, `DELETE FROM users WHERE email = $1`, email) })

	ch := mobChallenge(mobVerifier)
	state, nonce := startMob(t, e, "?client=mobile&code_challenge="+ch)
	w := e.do(http.MethodGet, "/auth/google/callback?code=c&state="+url.QueryEscape(state), "", nil, nonce)
	require.Equal(t, http.StatusFound, w.Code)
	loc, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "mypersonal", loc.Scheme)
	assert.Equal(t, "oauth", loc.Host)
	assert.Equal(t, "", loc.Path)
	mc, err := svc.VerifyMobileCode(loc.Query().Get("code"))
	require.NoError(t, err)
	assert.Equal(t, ch, mc.Challenge)
	assert.Nil(t, cookieNamed(w, "refresh_token"))

	// allowlist still applies
	ah.SetGoogle(svc, "http://front", []string{"other@example.com"}, false)
	state, nonce = startMob(t, e, "?client=mobile&code_challenge="+ch)
	w = e.do(http.MethodGet, "/auth/google/callback?code=c&state="+url.QueryEscape(state), "", nil, nonce)
	assert.Equal(t, "mypersonal://oauth?error=not_allowed", w.Header().Get("Location"))
}

func exchBody(code, verifier string) string {
	return `{"code":"` + code + `","code_verifier":"` + verifier + `"}`
}

func TestMobileExchange(t *testing.T) {
	e := newMobEnv(t)
	uid := uuid.New()
	ch := mobChallenge(mobVerifier)
	code, err := e.svc.SignMobileCode(uid, ch)
	require.NoError(t, err)

	// wrong verifier (well-formed, malformed) -> 401 and the code is not burned
	other := strings.Repeat("a", 43)
	for _, v := range []string{other, "short", ""} {
		w := e.do(http.MethodPost, "/auth/mobile/exchange", exchBody(code, v), nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), "invalid_code")
	}
	assert.Empty(t, e.once.seen)

	w := e.do(http.MethodPost, "/auth/mobile/exchange", exchBody(code, mobVerifier), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	d := decodeData[struct {
		AccessToken  string    `json:"access_token"`
		ExpiresAt    time.Time `json:"expires_at"`
		RefreshToken string    `json:"refresh_token"`
	}](t, w)
	ac, err := e.jwt.Verify(d.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, pkgjwt.AccessToken, ac.TokenType)
	assert.Equal(t, uid, ac.UserID)
	rc, err := e.jwt.Verify(d.RefreshToken)
	require.NoError(t, err)
	assert.Equal(t, pkgjwt.RefreshToken, rc.TokenType)
	assert.WithinDuration(t, time.Now().Add(15*time.Minute), d.ExpiresAt, time.Minute)

	// reuse
	w = e.do(http.MethodPost, "/auth/mobile/exchange", exchBody(code, mobVerifier), nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMobileExchangeRejectsBadCodes(t *testing.T) {
	e := newMobEnv(t)
	ch := mobChallenge(mobVerifier)

	// wrong purpose: a login state and an access token signed with the same secret
	st, _ := startMob(t, e, "?client=mobile&code_challenge="+ch)
	acc, err := e.jwt.GenerateAccess(uuid.New())
	require.NoError(t, err)
	for name, code := range map[string]string{"state": st, "access": acc, "garbage": "x.y.z", "empty": ""} {
		w := e.do(http.MethodPost, "/auth/mobile/exchange", exchBody(code, mobVerifier), nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code, name)
	}

	// tampered signature
	good, _ := e.svc.SignMobileCode(uuid.New(), ch)
	w := e.do(http.MethodPost, "/auth/mobile/exchange", exchBody(good[:len(good)-2]+"AA", mobVerifier), nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	w = e.do(http.MethodPost, "/auth/mobile/exchange", `not json`, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMobileExchangeExpired(t *testing.T) {
	e := newMobEnv(t)
	code, err := googlecal.SignMobileCodeTTL(stateSecret, uuid.New(), mobChallenge(mobVerifier), -time.Minute)
	require.NoError(t, err)
	w := e.do(http.MethodPost, "/auth/mobile/exchange", exchBody(code, mobVerifier), nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMobileExchangeNeedsRedis(t *testing.T) {
	e := newMobEnv(t)
	code, _ := e.svc.SignMobileCode(uuid.New(), mobChallenge(mobVerifier))

	e.ah.SetMobile(nil)
	w := e.do(http.MethodPost, "/auth/mobile/exchange", exchBody(code, mobVerifier), nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "mobile_login_unavailable")

	e.once.err = assert.AnError
	e.ah.SetMobile(e.once)
	w = e.do(http.MethodPost, "/auth/mobile/exchange", exchBody(code, mobVerifier), nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	// a bad code is still 401, not 503, even without Redis
	e.ah.SetMobile(nil)
	w = e.do(http.MethodPost, "/auth/mobile/exchange", exchBody("bad", mobVerifier), nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRefreshViaBody(t *testing.T) {
	e := newMobEnv(t)
	uid := uuid.New()
	rt, err := e.jwt.GenerateRefresh(uid)
	require.NoError(t, err)
	at, _ := e.jwt.GenerateAccess(uid)
	body := `{"refresh_token":"` + rt + `"}`

	w := e.do(http.MethodPost, "/auth/refresh", body, map[string]string{"X-Client": "mobile"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	d := decodeData[tokenResponse](t, w)
	c, err := e.jwt.Verify(d.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, uid, c.UserID)
	assert.NotContains(t, w.Body.String(), "refresh_token")

	// without the header the body is ignored (cookie path -> 401)
	w = e.do(http.MethodPost, "/auth/refresh", body, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// an access token is not a refresh token
	w = e.do(http.MethodPost, "/auth/refresh", `{"refresh_token":"`+at+`"}`, map[string]string{"X-Client": "mobile"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	w = e.do(http.MethodPost, "/auth/refresh", `{}`, map[string]string{"X-Client": "mobile"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// cookie path unchanged and wins over the body
	other, _ := e.jwt.GenerateRefresh(uuid.New())
	w = e.do(http.MethodPost, "/auth/refresh", `{"refresh_token":"`+other+`"}`, map[string]string{"X-Client": "mobile"},
		&http.Cookie{Name: "refresh_token", Value: rt})
	require.Equal(t, http.StatusOK, w.Code)
	c, _ = e.jwt.Verify(decodeData[tokenResponse](t, w).AccessToken)
	assert.Equal(t, uid, c.UserID)
}

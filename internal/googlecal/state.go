package googlecal

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	statePurpose      = "google_oauth"
	loginStatePurpose = "google_login"
	mobileCodePurpose = "mobile_code"
	MobileCodeTTL     = 2 * time.Minute
	ClientMobile      = "mobile"
	StateTTL          = 10 * time.Minute
)

type stateClaims struct {
	Purpose   string `json:"purpose"`
	Client    string `json:"client,omitempty"`    // "mobile" on app-initiated login
	Challenge string `json:"challenge,omitempty"` // PKCE S256 challenge (mobile only)
	jwt.RegisteredClaims
}

// stateKey derives a key distinct from the access-token secret, so a state
// token can never be replayed as a session token or vice versa.
func stateKey(secret, purpose string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(purpose))
	return m.Sum(nil)
}

// SignState issues the OAuth `state` JWT binding the callback to a user.
func SignState(secret string, userID uuid.UUID, ttl time.Duration) (string, error) {
	return signState(secret, statePurpose, userID.String(), ttl)
}

func signState(secret, purpose, subject string, ttl time.Duration) (string, error) {
	return signClaims(secret, stateClaims{Purpose: purpose, RegisteredClaims: jwt.RegisteredClaims{Subject: subject}}, ttl)
}

func signClaims(secret string, cl stateClaims, ttl time.Duration) (string, error) {
	now := time.Now()
	cl.IssuedAt = jwt.NewNumericDate(now)
	cl.ExpiresAt = jwt.NewNumericDate(now.Add(ttl))
	return jwt.NewWithClaims(jwt.SigningMethodHS256, cl).SignedString(stateKey(secret, cl.Purpose))
}

func verifyState(secret, purpose, token string) (string, error) {
	cl, err := verifyClaims(secret, purpose, token)
	if err != nil {
		return "", err
	}
	return cl.Subject, nil
}

func verifyClaims(secret, purpose, token string) (stateClaims, error) {
	var cl stateClaims
	_, err := jwt.ParseWithClaims(token, &cl, func(t *jwt.Token) (any, error) {
		return stateKey(secret, purpose), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		return stateClaims{}, err
	}
	if cl.Purpose != purpose {
		return stateClaims{}, errors.New("wrong purpose")
	}
	return cl, nil
}

// MobileCode is the verified content of a one-time mobile login code.
type MobileCode struct {
	UserID    uuid.UUID
	Challenge string
	JTI       string
}

// VerifyState checks signature, expiry and purpose, and returns the user id.
func VerifyState(secret, token string) (uuid.UUID, error) {
	sub, err := verifyState(secret, statePurpose, token)
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(sub)
}

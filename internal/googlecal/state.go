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
	StateTTL          = 10 * time.Minute
)

type stateClaims struct {
	Purpose string `json:"purpose"`
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
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, stateClaims{
		Purpose: purpose,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}).SignedString(stateKey(secret, purpose))
}

func verifyState(secret, purpose, token string) (string, error) {
	var cl stateClaims
	_, err := jwt.ParseWithClaims(token, &cl, func(t *jwt.Token) (any, error) {
		return stateKey(secret, purpose), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		return "", err
	}
	if cl.Purpose != purpose {
		return "", errors.New("wrong purpose")
	}
	return cl.Subject, nil
}

// VerifyState checks signature, expiry and purpose, and returns the user id.
func VerifyState(secret, token string) (uuid.UUID, error) {
	sub, err := verifyState(secret, statePurpose, token)
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(sub)
}

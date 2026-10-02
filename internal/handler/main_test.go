package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	pkgjwt "github.com/chronaxis/daily-planner-backend/pkg/jwt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// Integration tests run against a real Postgres (Architechture.md §13).
// Set TEST_DATABASE_URL to enable; otherwise every test skips, so `go test ./...`
// stays green on a machine without services.
var (
	testDB  *pgxpool.Pool
	testJWT *pkgjwt.Manager
	testCtx = context.Background()
)

const migrationsDir = "../../migrations"

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run())
	}

	pool, err := pgxpool.New(testCtx, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test db connect: %v\n", err)
		os.Exit(1)
	}
	if err := pool.Ping(testCtx); err != nil {
		fmt.Fprintf(os.Stderr, "test db ping: %v\n", err)
		os.Exit(1)
	}
	if err := applyMigrations(pool); err != nil {
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
		os.Exit(1)
	}

	testDB = pool
	testJWT = pkgjwt.NewManager(strings.Repeat("test-secret-", 4), 15*time.Minute, time.Hour)

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// applyMigrations runs every migrations/*.sql in lexical order. All statements
// are idempotent, so re-running against an existing dev DB is safe.
func applyMigrations(pool *pgxpool.Pool) error {
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(testCtx, string(raw)); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func requireDB(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
}

// newUser inserts a throwaway user and returns its id plus a valid access
// token. The row is deleted (cascading) when the test ends.
func newUser(t *testing.T) (uuid.UUID, string) {
	t.Helper()

	var id uuid.UUID
	err := testDB.QueryRow(testCtx, `
		INSERT INTO users (name, email, password_hash)
		VALUES ('Test User', $1, 'x')
		RETURNING id
	`, fmt.Sprintf("t-%s@example.com", uuid.NewString())).Scan(&id)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = testDB.Exec(testCtx, `DELETE FROM users WHERE id = $1`, id)
	})

	token, err := testJWT.GenerateAccess(id)
	require.NoError(t, err)

	return id, token
}

// newProtectedRouter mounts routes behind the real auth middleware so tests
// exercise the same path production does.
func newProtectedRouter(mount func(g *gin.RouterGroup)) *gin.Engine {
	r := gin.New()
	g := r.Group("")
	g.Use(middleware.Auth(testJWT))
	mount(g)
	return r
}

// doJSON performs a request with an optional JSON body and Bearer token.
func doJSON(t *testing.T, r http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = strings.NewReader(string(raw))
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// decodeData unmarshals the `data` field of the response envelope.
func decodeData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()

	var env struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "body: %s", w.Body.String())
	return env.Data
}

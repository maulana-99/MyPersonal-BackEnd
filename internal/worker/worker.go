// Package worker consumes Asynq tasks. For MVP the only task is reminder
// delivery, which materialises a row in the notifications table.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/chronaxis/daily-planner-backend/internal/queue"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Server wraps an Asynq server bound to a Postgres pool.
type Server struct {
	srv *asynq.Server
	mux *asynq.ServeMux
	db  *pgxpool.Pool
}

// New builds a worker that reads reminders from Redis and writes notifications
// to Postgres. redisOpt points at the same Redis the enqueue side uses.
func New(redisOpt asynq.RedisClientOpt, db *pgxpool.Pool) *Server {
	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: 5,
		Queues:      map[string]int{"default": 1},
	})

	s := &Server{srv: srv, mux: asynq.NewServeMux(), db: db}
	s.mux.HandleFunc(queue.TypeSendReminder, s.handleSendReminder)
	return s
}

// Run blocks serving until the server is shut down or ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.srv.Shutdown()
	}()
	if err := s.srv.Run(s.mux); err != nil {
		return fmt.Errorf("asynq run: %w", err)
	}
	return nil
}

// Shutdown stops accepting new tasks and waits for in-flight ones.
func (s *Server) Shutdown() {
	s.srv.Shutdown()
}

func (s *Server) handleSendReminder(ctx context.Context, t *asynq.Task) error {
	var p queue.ReminderPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		// Malformed payload will never succeed on retry — drop it.
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}

	body := "Starting now"
	if p.OffsetMins > 0 {
		body = fmt.Sprintf("Starts in %d minutes", p.OffsetMins)
	}

	_, err := s.db.Exec(ctx, `
		INSERT INTO notifications (user_id, title, body)
		VALUES ($1, $2, $3)
	`, p.UserID, p.ScheduleName, body)
	if err != nil {
		return fmt.Errorf("insert notification: %w", err)
	}

	log.Printf("worker: reminder delivered user=%s schedule=%q", p.UserID, p.ScheduleName)
	return nil
}

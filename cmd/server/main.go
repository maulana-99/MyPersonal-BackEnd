package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/config"
	"github.com/chronaxis/daily-planner-backend/internal/handler"
	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/internal/queue"
	"github.com/chronaxis/daily-planner-backend/internal/scheduler"
	"github.com/chronaxis/daily-planner-backend/internal/worker"
	pkgjwt "github.com/chronaxis/daily-planner-backend/pkg/jwt"
	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// DB
	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer db.Close()

	if err = db.Ping(context.Background()); err != nil {
		log.Fatalf("db ping: %v", err)
	}

	// JWT
	jwtMgr := pkgjwt.NewManager(cfg.JWTSecret, cfg.JWTAccessExpiry, cfg.JWTRefreshExpiry)

	// Router
	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()

	// CORS — ponytail: no cors lib; tighten origins when frontend domain known
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "http://localhost:5173")
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})

	// Queue + workers are optional: API-only mode (ENABLE_WORKER=false) keeps
	// tests and local API work fast without Redis.
	enableWorker := true
	if raw := os.Getenv("ENABLE_WORKER"); raw != "" {
		if b, err := strconv.ParseBool(raw); err == nil {
			enableWorker = b
		}
	}

	var queueClient *queue.Client
	if enableWorker {
		queueClient = queue.NewClient(cfg.RedisAddr, cfg.RedisPassword)
		defer queueClient.Close()
	}

	// Handlers
	authHandler := handler.NewAuthHandler(db, jwtMgr)
	categoryHandler := handler.NewCategoryHandler(db)
	scheduleHandler := handler.NewScheduleHandler(db, queueClient)
	calendarHandler := handler.NewCalendarHandler(db)
	todayHandler := handler.NewTodayHandler(db)
	taskHandler := handler.NewTaskHandler(db)
	routineHandler := handler.NewRoutineHandler(db)
	notificationHandler := handler.NewNotificationHandler(db)
	statisticsHandler := handler.NewStatisticsHandler(db)
	habitHandler := handler.NewHabitHandler(db)
	focusHandler := handler.NewFocusHandler(db)
	noteHandler := handler.NewNoteHandler(db)
	reviewHandler := handler.NewReviewHandler(db)
	focusRuleHandler := handler.NewFocusRuleHandler(db)
	searchHandler := handler.NewSearchHandler(db)

	v1 := r.Group("/api/v1")
	{
		auth := v1.Group("/auth")
		{
			auth.POST("/register", authHandler.Register)
			auth.POST("/login", authHandler.Login)
			auth.POST("/refresh", authHandler.Refresh)
			auth.POST("/logout", authHandler.Logout)
		}

		// Protected routes
		protected := v1.Group("")
		protected.Use(middleware.Auth(jwtMgr))
		{
			protected.GET("/users/me", authHandler.Me)

			protected.GET("/categories", categoryHandler.List)
			protected.POST("/categories", categoryHandler.Create)
			protected.PUT("/categories/:id", categoryHandler.Update)
			protected.DELETE("/categories/:id", categoryHandler.Delete)

			protected.GET("/schedules", scheduleHandler.List)
			protected.POST("/schedules", scheduleHandler.Create)
			protected.GET("/schedules/:id", scheduleHandler.Get)
			protected.PUT("/schedules/:id", scheduleHandler.Update)
			protected.DELETE("/schedules/:id", scheduleHandler.Delete)
			protected.POST("/schedules/:id/complete", scheduleHandler.Complete)
			protected.POST("/schedules/:id/skip", scheduleHandler.Skip)
			protected.POST("/schedules/:id/duplicate", scheduleHandler.Duplicate)
			protected.POST("/schedules/:id/move", scheduleHandler.Move)
			protected.GET("/schedules/:id/reminders", scheduleHandler.Reminders)
			protected.POST("/schedules/:id/reminders/:rid/snooze", scheduleHandler.Snooze)
			protected.POST("/schedules/:id/reminders/:rid/dismiss", scheduleHandler.DismissReminder)

			protected.GET("/calendar", calendarHandler.List)
			protected.GET("/today", todayHandler.Get)

			protected.GET("/tasks", taskHandler.List)
			protected.POST("/tasks", taskHandler.Create)
			protected.GET("/tasks/:id", taskHandler.Get)
			protected.PUT("/tasks/:id", taskHandler.Update)
			protected.DELETE("/tasks/:id", taskHandler.Delete)
			protected.POST("/tasks/:id/complete", taskHandler.Complete)
			protected.POST("/tasks/:id/subtasks", taskHandler.AddSubtask)
			protected.PUT("/tasks/:id/subtasks/:sid", taskHandler.UpdateSubtask)
			protected.DELETE("/tasks/:id/subtasks/:sid", taskHandler.DeleteSubtask)

			protected.GET("/routines", routineHandler.List)
			protected.POST("/routines", routineHandler.Create)
			protected.GET("/routines/:id", routineHandler.Get)
			protected.PUT("/routines/:id", routineHandler.Update)
			protected.DELETE("/routines/:id", routineHandler.Delete)
			protected.PUT("/routines/:id/items/order", routineHandler.Reorder)
			protected.POST("/routines/:id/items/:itemId/toggle", routineHandler.ToggleItem)

			protected.GET("/notifications", notificationHandler.List)
			protected.POST("/notifications/:id/read", notificationHandler.Read)
			protected.POST("/notifications/:id/dismiss", notificationHandler.Dismiss)
			protected.POST("/notifications/read-all", notificationHandler.ReadAll)

			protected.GET("/statistics", statisticsHandler.Get)

			// ---- V2 ----
			// One wildcard name per position: :id for the resource, :sid for a
			// sub-resource, :date for a day. Gin panics at boot on two names in
			// the same position, so sub-resources never invent their own name.

			protected.GET("/habits", habitHandler.List)
			protected.POST("/habits", habitHandler.Create)
			protected.GET("/habits/history", habitHandler.History)
			protected.GET("/habits/:id", habitHandler.Get)
			protected.PUT("/habits/:id", habitHandler.Update)
			protected.DELETE("/habits/:id", habitHandler.Delete)
			protected.POST("/habits/:id/complete", habitHandler.Complete)
			protected.GET("/habits/:id/logs", habitHandler.Logs)

			protected.GET("/focus-sessions", focusHandler.List)
			protected.POST("/focus-sessions", focusHandler.Start)
			protected.GET("/focus-sessions/active", focusHandler.Active)
			protected.GET("/focus-sessions/stats", focusHandler.Stats)
			protected.GET("/focus-sessions/:id", focusHandler.Get)
			protected.DELETE("/focus-sessions/:id", focusHandler.Delete)
			protected.POST("/focus-sessions/:id/pause", focusHandler.Pause)
			protected.POST("/focus-sessions/:id/resume", focusHandler.Resume)
			protected.POST("/focus-sessions/:id/complete", focusHandler.Complete)
			protected.POST("/focus-sessions/:id/cancel", focusHandler.Cancel)

			protected.GET("/notes", noteHandler.List)
			protected.POST("/notes", noteHandler.Create)
			protected.GET("/notes/:id", noteHandler.Get)
			protected.PUT("/notes/:id", noteHandler.Update)
			protected.DELETE("/notes/:id", noteHandler.Delete)

			protected.GET("/daily-reviews", reviewHandler.List)
			protected.GET("/daily-reviews/:date", reviewHandler.Get)
			protected.PUT("/daily-reviews/:date", reviewHandler.Upsert)

			protected.GET("/focus-rules", focusRuleHandler.List)
			protected.POST("/focus-rules", focusRuleHandler.Create)
			protected.GET("/focus-rules/active", focusRuleHandler.Active)
			protected.GET("/focus-rules/:id", focusRuleHandler.Get)
			protected.PUT("/focus-rules/:id", focusRuleHandler.Update)
			protected.DELETE("/focus-rules/:id", focusRuleHandler.Delete)

			protected.GET("/search", searchHandler.Search)
		}
	}

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	// Background jobs: reminder scheduler + Asynq worker. Both stop with the
	// same context the HTTP server shuts down on.
	jobCtx, stopJobs := context.WithCancel(context.Background())
	defer stopJobs()

	if enableWorker {
		if queueClient == nil || cfg.RedisAddr == "" {
			log.Fatal("ENABLE_WORKER=true requires REDIS_ADDR")
		}
		redisOpt := asynq.RedisClientOpt{Addr: cfg.RedisAddr, Password: cfg.RedisPassword}
		w := worker.New(redisOpt, db)
		go func() {
			if err := w.Run(jobCtx); err != nil {
				log.Printf("worker stopped: %v", err)
			}
		}()
		go scheduler.Run(jobCtx, db, queueClient, scheduler.DefaultInterval)
	}

	go func() {
		log.Printf("server listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	stopJobs()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
	log.Println("server exited")
}

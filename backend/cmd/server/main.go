package main

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"koplink/internal/api"
	"koplink/internal/database"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"
)

func main() {
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is required")
	}
	p, e := pgxpool.New(ctx, url)
	if e != nil {
		log.Fatal(e)
	}
	defer p.Close()
	if e = p.Ping(ctx); e != nil {
		log.Fatal(e)
	}
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "migrate":
		if e = database.Migrate(ctx, p); e != nil {
			log.Fatal(e)
		}
		log.Print("Migrations completed")
		return
	case "bootstrap":
		if e = database.Bootstrap(ctx, p, os.Getenv("INITIAL_ADMIN_EMAIL"), os.Getenv("INITIAL_ADMIN_PASSWORD")); e != nil {
			log.Fatal(e)
		}
		log.Print("Initial administrator ready; configure financial policies before use")
		return
	case "seed":
		if os.Getenv("DEMO_MODE") != "true" {
			log.Fatal("Seed requires DEMO_MODE=true; demo rules and identities are fictional")
		}
		if e = database.Seed(ctx, p, os.Getenv("DEMO_PASSWORD")); e != nil {
			log.Fatal(e)
		}
		log.Print("DEMO seed completed; all demo policies are fictional")
		return
	case "serve":
	default:
		log.Fatal("command must be serve, migrate, bootstrap or seed")
	}
	secret := os.Getenv("JWT_SECRET")
	if len(secret) < 32 {
		log.Fatal("JWT_SECRET must have at least 32 bytes")
	}
	uploads := os.Getenv("UPLOAD_DIR")
	if uploads == "" {
		uploads = "uploads"
	}
	a := &api.API{DB: p, Secret: []byte(secret), Demo: os.Getenv("DEMO_MODE") == "true", UploadDir: uploads, Origin: os.Getenv("CORS_ORIGIN")}
	app := a.App()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-stop; _ = app.Shutdown() }()
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			if _, e := a.RunScheduler(); e != nil {
				log.Printf("scheduler: %v", e)
			}
			<-ticker.C
		}
	}()
	log.Fatal(app.Listen(":" + port))
}

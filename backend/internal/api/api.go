package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"log"
	"strconv"
	"time"
)

type API struct {
	DB        *pgxpool.Pool
	Secret    []byte
	Demo      bool
	UploadDir string
	Origin    string
}

func ok(c *fiber.Ctx, v any) error { return c.JSON(fiber.Map{"data": v}) }
func fail(c *fiber.Ctx, status int, code, msg string) error {
	return c.Status(status).JSON(fiber.Map{"error": fiber.Map{"code": code, "message": msg}})
}
func actor(c *fiber.Ctx) int64  { v, _ := c.Locals("user_id").(int64); return v }
func member(c *fiber.Ctx) int64 { v, _ := c.Locals("member_id").(int64); return v }
func role(c *fiber.Ctx) string  { v, _ := c.Locals("role").(string); return v }
func require(roles ...string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		for _, r := range roles {
			if role(c) == r {
				return c.Next()
			}
		}
		return fail(c, 403, "forbidden", "Akses tidak diizinkan")
	}
}
func id(c *fiber.Ctx) int64 { v, _ := strconv.ParseInt(c.Params("id"), 10, 64); return v }
func randomID() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func (a *API) rows(c *fiber.Ctx, q string, args ...any) error {
	rs, e := a.DB.Query(c.Context(), q, args...)
	if e != nil {
		return e
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		v, e := rs.Values()
		if e != nil {
			return e
		}
		m := map[string]any{}
		for i, f := range rs.FieldDescriptions() {
			m[f.Name] = v[i]
		}
		out = append(out, m)
	}
	if e = rs.Err(); e != nil {
		return e
	}
	return ok(c, out)
}
func audit(ctx context.Context, tx pgx.Tx, uid int64, action, entity string, id int64, before, after any) error {
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	_, e := tx.Exec(ctx, `INSERT INTO audit_logs(actor_id,action,entity,entity_id,"before","after") VALUES(NULLIF($1,0),$2,$3,$4,$5,$6)`, uid, action, entity, id, b, a)
	return e
}
func journal(ctx context.Context, tx pgx.Tx, ref, debit, credit string, amount, mid int64, desc string) error {
	if amount == 0 {
		return nil
	}
	if amount < 0 {
		return errors.New("negative journal")
	}
	_, e := tx.Exec(ctx, `INSERT INTO ledger(reference,account,debit,credit,member_id,description) VALUES($1,$2,$4,0,NULLIF($5,0),$6),($1,$3,0,$4,NULLIF($5,0),$6)`, ref, debit, credit, amount, mid, desc)
	return e
}
func (a *API) active(c *fiber.Ctx, mid int64) error {
	var s string
	e := a.DB.QueryRow(c.Context(), "SELECT status FROM members WHERE id=$1", mid).Scan(&s)
	if e != nil || s != "active" {
		return fiber.NewError(403, "Keanggotaan harus aktif")
	}
	return nil
}
func (a *API) App() *fiber.App {
	app := fiber.New(fiber.Config{BodyLimit: 17 * 1024 * 1024, ErrorHandler: func(c *fiber.Ctx, e error) error {
		var fe *fiber.Error
		if errors.As(e, &fe) {
			return fail(c, fe.Code, "request_failed", fe.Message)
		}
		log.Printf("request error: %v", e)
		var pe *pgconn.PgError
		if errors.As(e, &pe) {
			switch pe.Code {
			case "23505":
				return fail(c, 409, "duplicate", "Data atau referensi sudah digunakan")
			case "23503", "23514", "22P02", "22007":
				return fail(c, 422, "invalid_data", "Data tidak memenuhi aturan")
			}
		}
		return fail(c, 500, "internal_error", "Operasi gagal karena gangguan internal")
	}})
	app.Use(recover.New())
	if a.Origin != "" {
		app.Use(cors.New(cors.Config{AllowOrigins: a.Origin, AllowHeaders: "Origin, Content-Type, Accept, Authorization"}))
	}
	app.Get("/health", func(c *fiber.Ctx) error {
		if e := a.DB.Ping(c.Context()); e != nil {
			return fail(c, 503, "database_unavailable", "Database tidak tersedia")
		}
		return ok(c, fiber.Map{"status": "ok"})
	})
	r := app.Group("/api/v1")
	r.Get("/meta", func(c *fiber.Ctx) error {
		return ok(c, fiber.Map{"demo": a.Demo, "payment_mode": "manual_verification", "timezone": "Asia/Makassar"})
	})
	r.Post("/auth/login", limiter.New(limiter.Config{Max: 10, Expiration: time.Minute}), a.login)
	r.Post("/auth/register", limiter.New(limiter.Config{Max: 10, Expiration: time.Minute}), a.register)
	r.Post("/auth/forgot-password", func(c *fiber.Ctx) error {
		return fail(c, 503, "delivery_not_configured", "Pengiriman reset email belum dikonfigurasi; hubungi administrator")
	})
	r.Post("/auth/reset-password", func(c *fiber.Ctx) error {
		return fail(c, 503, "delivery_not_configured", "Layanan token reset belum dikonfigurasi")
	})
	r.Use(a.authenticate)
	a.authRoutes(r)
	a.memberProfileRoutes(r)
	a.tierRoutes(r)
	a.depositRoutes(r)
	a.creditExtensionRoutes(r)
	a.savingsRoutes(r)
	a.loanRoutes(r)
	a.reporting(r)
	a.documents(r)
	a.dashboardRoutes(r)
	return app
}

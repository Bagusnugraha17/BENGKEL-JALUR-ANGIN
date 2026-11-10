package api

import (
	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"koplink/internal/finance"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthenticationRejectsInvalidTokensBeforeDatabase(t *testing.T) {
	a := &API{Secret: []byte("test-only-long-secret-1234567890123456")}
	app := a.App()
	expired, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix(), "jti": "invalid"}).SignedString(a.Secret)
	wrong, _ := jwt.NewWithClaims(jwt.SigningMethodHS384, jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()}).SignedString(a.Secret)
	for _, token := range []string{"", "garbage", expired, wrong} {
		r := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		resp, e := app.Test(r)
		if e != nil || resp.StatusCode != 401 {
			t.Fatalf("token rejected: response=%v error=%v", resp, e)
		}
	}
}
func TestRoleMiddleware(t *testing.T) {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error { c.Locals("role", c.Get("X-Test-Role")); return c.Next() })
	app.Get("/admin", require("admin"), func(c *fiber.Ctx) error { return c.SendStatus(200) })
	for _, role := range []string{"member", "teller", "committee", "admin", ""} {
		req := httptest.NewRequest("GET", "/admin", nil)
		req.Header.Set("X-Test-Role", role)
		resp, e := app.Test(req)
		if e != nil {
			t.Fatal(e)
		}
		want := 403
		if role == "admin" {
			want = 200
		}
		if resp.StatusCode != want {
			t.Fatalf("%s got %d", role, resp.StatusCode)
		}
	}
}
func TestPasswordBounds(t *testing.T) {
	if validPassword("short") || validPassword(string(make([]byte, 73))) || !validPassword("ExamplePass!2026") {
		t.Fatal("password bounds")
	}
}

func TestUsernameValidation(t *testing.T) {
	for _, value := range []string{"ab", "has space", "name/invalid", "", strings.Repeat("a", 33)} {
		if validUsername(value) {
			t.Fatalf("invalid username accepted: %q", value)
		}
	}
	for _, value := range []string{"bagus_01", "kop.link-2026", "User1234"} {
		if !validUsername(value) {
			t.Fatalf("valid username rejected: %q", value)
		}
	}
}

func TestPostgresCivilDatePenaltyAtMakassarMidnight(t *testing.T) {
	loc, e := time.LoadLocation("Asia/Makassar")
	if e != nil {
		t.Fatal(e)
	}
	due := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 0, 1, 0, 0, loc)
	if got := finance.Penalty(100000, 1000, 0, localDueDate(due, loc), now); got != 1000 {
		t.Fatalf("UTC-scanned DATE must accrue at Makassar midnight: %d", got)
	}
}

func TestLoanApplicationRequiresDocumentsBeforeDatabase(t *testing.T) {
	a := &API{}
	app := fiber.New()
	app.Post("/apply", a.applyLoan)
	for _, body := range []string{`{"product_id":1,"amount":1000000,"tenor":6,"purpose":"Modal"}`, `{"product_id":1,"amount":1000000,"tenor":6,"purpose":"Modal","document_ids":[]}`} {
		r := httptest.NewRequest("POST", "/apply", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		res, e := app.Test(r)
		if e != nil {
			t.Fatal(e)
		}
		if res.StatusCode != 400 {
			t.Fatalf("missing loan documents got %d", res.StatusCode)
		}
		res.Body.Close()
	}
}

func TestStaffUserCreationRejectsInvalidInputBeforeDatabase(t *testing.T) {
	a := &API{}
	app := fiber.New()
	app.Post("/users", a.createStaffUser)
	cases := []string{
		`{}`,
		`{"name":"A","email":"not-an-email","password":"StrongPassword!2026","role":"teller"}`,
		`{"name":"A","email":"staff@example.test","password":"short","role":"teller"}`,
		`{"name":"A","email":"staff@example.test","password":"StrongPassword!2026","role":"member"}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest("POST", "/users", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 400 {
			t.Fatalf("invalid staff input should return 400; body=%s status=%d", body, resp.StatusCode)
		}
	}
}

func TestMemberRegistrationRejectsInvalidInputBeforeDatabase(t *testing.T) {
	a := &API{}
	app := fiber.New()
	app.Post("/register", a.register)
	cases := []string{
		`{}`,
		`{"name":"Test","email":"bad","password":"StrongPassword!2026","nik":"1234567890123456","address":"Alamat","phone":"08123456789"}`,
		`{"name":"Test","email":"member@example.test","password":"short","nik":"1234567890123456","address":"Alamat","phone":"08123456789"}`,
		`{"name":"Test","email":"member@example.test","password":"StrongPassword!2026","nik":"123","address":"Alamat","phone":"08123456789"}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 400 {
			t.Fatalf("invalid registration should return 400; body=%s status=%d", body, resp.StatusCode)
		}
	}
}

func TestMemberRegistrationRequiresIdentityDocument(t *testing.T) {
	a := &API{}
	app := fiber.New()
	app.Post("/register", a.register)
	body := `{"name":"Test Member","email":"member@example.test","password":"StrongPassword!2026","nik":"1234567890123456","address":"Alamat","phone":"08123456789"}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("registration without identity document should return 400, got %d", resp.StatusCode)
	}
}

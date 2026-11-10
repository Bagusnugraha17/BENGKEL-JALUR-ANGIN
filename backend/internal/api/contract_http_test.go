package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestPublicMetadataAndAuthenticationBoundary(t *testing.T) {
	app := (&API{Secret: []byte("unit-test-secret-at-least-32-bytes-long"), Demo: true}).App()
	defer app.Shutdown()
	res, err := app.Test(httptest.NewRequest("GET", "/api/v1/meta", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("meta status %d", res.StatusCode)
	}
	var got struct {
		Data struct {
			Demo bool   `json:"demo"`
			Mode string `json:"payment_mode"`
		} `json:"data"`
	}
	if err = json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Data.Demo || got.Data.Mode != "manual_verification" {
		t.Fatalf("metadata must not claim live gateway: %+v", got)
	}
	for _, path := range []string{"/auth/me", "/members", "/users", "/savings", "/savings-transactions", "/loan-applications", "/installments", "/payments", "/reports", "/audit-logs", "/documents", "/settings", "/shu-periods"} {
		r, e := app.Test(httptest.NewRequest("GET", "/api/v1"+path, nil))
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 401 {
			t.Errorf("%s accepted unauthenticated: %d", path, r.StatusCode)
		}
	}
}
func TestMalformedJWTRejectedBeforeDatabase(t *testing.T) {
	app := (&API{Secret: []byte("unit-test-secret-at-least-32-bytes-long")}).App()
	defer app.Shutdown()
	for _, token := range []string{"Bearer nonsense", "Basic abc", "Bearer eyJhbGciOiJub25lIn0.eyJleHAiOjQ3MDAwMDAwMDB9.", "Bearer "} {
		req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
		req.Header.Set("Authorization", token)
		r, e := app.Test(req)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 401 {
			t.Fatalf("invalid JWT accepted: %q, status %d", token, r.StatusCode)
		}
	}
}
func TestInvalidRegistrationNeverTouchesDatabase(t *testing.T) {
	app := (&API{Secret: []byte("unit-test-secret-at-least-32-bytes-long")}).App()
	defer app.Shutdown()
	for _, body := range []string{`{}`, `{"name":"A","email":"x","password":"longpassword","nik":"1234567890123456"}`, `{"name":"A","email":"a@example.test","password":"short","nik":"1234567890123456"}`, `{"name":"A","email":"a@example.test","password":"longpassword","nik":"123456789012345x"}`} {
		req := httptest.NewRequest("POST", "/api/v1/auth/register", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		r, e := app.Test(req)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Fatalf("bad registration accepted: %s, status %d", body, r.StatusCode)
		}
	}
}

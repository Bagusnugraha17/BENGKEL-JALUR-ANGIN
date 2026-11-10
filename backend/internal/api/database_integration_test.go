package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"koplink/internal/database"
)

// This test requires a real PostgreSQL server and CREATE SCHEMA privilege.
// It never resets the supplied database: all writes use one isolated schema.
func TestPostgresSeedAndFinancialRollback(t *testing.T) {
	url := os.Getenv("KOPLINK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("Real PostgreSQL required: set KOPLINK_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	root, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	schema := fmt.Sprintf("koplink_test_%d", time.Now().UnixNano())
	if _, err = root.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer root.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
	password := "KopLinkDemo!2026"
	if err = database.Seed(ctx, pool, password); err != nil {
		t.Fatal(err)
	}
	var countBefore, countAfter int64
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM ledger").Scan(&countBefore); err != nil {
		t.Fatal(err)
	}
	if err = database.Seed(ctx, pool, password); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM ledger").Scan(&countAfter); err != nil {
		t.Fatal(err)
	}
	if countBefore != countAfter || countAfter == 0 {
		t.Fatalf("seed replay/empty journal: %d %d", countBefore, countAfter)
	}
	var mismatches int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM (SELECT reference FROM ledger GROUP BY reference HAVING sum(debit)<>sum(credit)) x").Scan(&mismatches); err != nil {
		t.Fatal(err)
	}
	if mismatches != 0 {
		t.Fatalf("unbalanced seed journal: %d", mismatches)
	}
	app := (&API{DB: pool, Secret: []byte("isolated-real-db-test-secret-at-least32"), Demo: true, UploadDir: t.TempDir()}).App()
	defer app.Shutdown()
	teller := dbTestLogin(t, app, "teller@koplink.demo", password)
	memberToken := dbTestLogin(t, app, "member@koplink.demo", password)
	var typeID, mid int64
	if err = pool.QueryRow(ctx, "SELECT id FROM savings_types WHERE code='voluntary'").Scan(&typeID); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT m.id FROM members m JOIN users u ON u.id=m.user_id WHERE u.email='member@koplink.demo'").Scan(&mid); err != nil {
		t.Fatal(err)
	}
	var balanceBefore int64
	if err = pool.QueryRow(ctx, "SELECT balance FROM savings WHERE member_id=$1 AND type_id=$2", mid, typeID).Scan(&balanceBefore); err != nil {
		t.Fatal(err)
	}
	code, response := dbTestRequest(t, app, "POST", "/savings-transactions", memberToken, map[string]any{"type_id": typeID, "kind": "deposit", "amount": 1000, "note": "rollback test", "reference": "rollback-fixture"})
	if code != 200 && code != 201 {
		t.Fatalf("create: %d %+v", code, response)
	}
	tid := int64(response["data"].(map[string]any)["id"].(float64))
	// Force failure after balance, journal, status and notification writes, at audit.
	_, err = pool.Exec(ctx, `CREATE FUNCTION reject_test_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='verify' THEN RAISE EXCEPTION 'injected test failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_test_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_test_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	code, _ = dbTestRequest(t, app, "POST", fmt.Sprintf("/savings-transactions/%d/verify", tid), teller, map[string]any{"approve": true, "reason": "rollback proof"})
	if code != 500 {
		t.Fatalf("expected sanitized 500, got %d", code)
	}
	var balanceAfter, journalCount int64
	var status string
	if err = pool.QueryRow(ctx, "SELECT balance FROM savings WHERE member_id=$1 AND type_id=$2", mid, typeID).Scan(&balanceAfter); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT status FROM savings_transactions WHERE id=$1", tid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM ledger WHERE reference=$1", fmt.Sprintf("saving:%d", tid)).Scan(&journalCount); err != nil {
		t.Fatal(err)
	}
	if balanceAfter != balanceBefore || status != "pending" || journalCount != 0 {
		t.Fatalf("partial commit: balance %d/%d status %s journal %d", balanceBefore, balanceAfter, status, journalCount)
	}
	if _, err = pool.Exec(ctx, "DROP TRIGGER reject_test_audit ON audit_logs"); err != nil {
		t.Fatal(err)
	}
	code, _ = dbTestRequest(t, app, "POST", fmt.Sprintf("/savings-transactions/%d/verify", tid), teller, map[string]any{"approve": true, "reason": "retry after transient failure"})
	if code != 200 {
		t.Fatalf("retry failed: %d", code)
	}
}
func dbTestLogin(t *testing.T, app *fiber.App, email, password string) string {
	t.Helper()
	code, response := dbTestRequest(t, app, "POST", "/auth/login", "", map[string]any{"email": email, "password": password})
	if code != 200 {
		t.Fatalf("login: %d %+v", code, response)
	}
	return response["data"].(map[string]any)["token"].(string)
}
func dbTestRequest(t *testing.T, app *fiber.App, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := app.Test(req, 10000)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err = json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, out
}

-- Role-specific workspaces and committee case assignment. Additive and reversible by retaining records.
CREATE TABLE IF NOT EXISTS loan_application_assignments (
  id bigserial PRIMARY KEY,
  application_id bigint NOT NULL REFERENCES loan_applications(id),
  committee_user_id bigint NOT NULL REFERENCES users(id),
  assigned_by bigint NOT NULL REFERENCES users(id),
  assigned_at timestamptz NOT NULL DEFAULT now(),
  unassigned_at timestamptz,
  reason text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS loan_application_one_active_assignment
  ON loan_application_assignments(application_id) WHERE unassigned_at IS NULL;
CREATE INDEX IF NOT EXISTS loan_application_assignment_user_idx
  ON loan_application_assignments(committee_user_id,assigned_at DESC) WHERE unassigned_at IS NULL;
CREATE TABLE IF NOT EXISTS committee_case_notes (
  id bigserial PRIMARY KEY,
  application_id bigint NOT NULL REFERENCES loan_applications(id),
  committee_user_id bigint NOT NULL REFERENCES users(id),
  note_type text NOT NULL CHECK(note_type IN ('document_review','analysis','survey','recommendation')),
  note text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE documents ADD COLUMN IF NOT EXISTS is_current boolean NOT NULL DEFAULT true;
CREATE INDEX IF NOT EXISTS documents_owner_kind_current_idx ON documents(user_id,kind,created_at DESC) WHERE is_current;
ALTER TABLE savings_transactions ADD COLUMN IF NOT EXISTS verified_at timestamptz;
CREATE INDEX IF NOT EXISTS savings_transactions_verified_at_idx ON savings_transactions(verified_at DESC) WHERE status='approved';

CREATE OR REPLACE FUNCTION koplink_prevent_locked_savings_withdrawal() RETURNS trigger AS $$
DECLARE locked_total bigint;
BEGIN
  SELECT COALESCE(sum(locked_amount),0)::bigint INTO locked_total
  FROM collateral_locks
  WHERE asset_type='savings' AND asset_id=OLD.id AND status='active';
  IF NEW.balance < locked_total THEN
    RAISE EXCEPTION 'saldo simpanan sedang dijaminkan sebesar %', locked_total USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='savings_locked_balance_guard' AND tgrelid='savings'::regclass AND NOT tgisinternal) THEN
    CREATE TRIGGER savings_locked_balance_guard BEFORE UPDATE OF balance ON savings
      FOR EACH ROW EXECUTE FUNCTION koplink_prevent_locked_savings_withdrawal();
  END IF;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS collateral_asset_one_active_lock
  ON collateral_locks(asset_type,asset_id) WHERE status='active';

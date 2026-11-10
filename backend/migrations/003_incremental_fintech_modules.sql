-- KopLink Stage 1-2: additive fintech modules. Existing rows and columns are retained.
ALTER TABLE members ADD COLUMN IF NOT EXISTS npwp text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS marital_status text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS domicile_address text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS bank_name text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS bank_account_number text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS bank_account_holder text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS emergency_contact_name text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS emergency_contact_phone text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS emergency_contact_relationship text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS heir_name text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS heir_phone text;
ALTER TABLE members ADD COLUMN IF NOT EXISTS mandatory_savings_amount bigint;
ALTER TABLE members ADD COLUMN IF NOT EXISTS initial_principal_amount bigint;
ALTER TABLE members ADD COLUMN IF NOT EXISTS consent_adart boolean NOT NULL DEFAULT false;
ALTER TABLE members ADD COLUMN IF NOT EXISTS consent_adart_at timestamptz;

ALTER TABLE documents ADD COLUMN IF NOT EXISTS review_status text NOT NULL DEFAULT 'pending';
ALTER TABLE documents ADD COLUMN IF NOT EXISTS review_reason text NOT NULL DEFAULT '';
ALTER TABLE documents ADD COLUMN IF NOT EXISTS reviewed_by bigint REFERENCES users(id);
ALTER TABLE documents ADD COLUMN IF NOT EXISTS reviewed_at timestamptz;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS expires_at date;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='documents_review_status_check' AND conrelid='documents'::regclass) THEN
    ALTER TABLE documents ADD CONSTRAINT documents_review_status_check CHECK (review_status IN ('pending','verified','rejected')) NOT VALID;
  END IF;
END $$;
ALTER TABLE documents VALIDATE CONSTRAINT documents_review_status_check;

ALTER TABLE loan_products ADD COLUMN IF NOT EXISTS max_ltv_bps integer;
ALTER TABLE loan_products ADD COLUMN IF NOT EXISTS interest_method_v2 text;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='loan_products_max_ltv_bps_check' AND conrelid='loan_products'::regclass) THEN
    ALTER TABLE loan_products ADD CONSTRAINT loan_products_max_ltv_bps_check CHECK (max_ltv_bps IS NULL OR max_ltv_bps BETWEEN 1 AND 10000) NOT VALID;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='loan_products_interest_method_v2_check' AND conrelid='loan_products'::regclass) THEN
    ALTER TABLE loan_products ADD CONSTRAINT loan_products_interest_method_v2_check CHECK (interest_method_v2 IS NULL OR interest_method_v2 IN ('flat','declining','annuity')) NOT VALID;
  END IF;
END $$;
ALTER TABLE loan_products VALIDATE CONSTRAINT loan_products_max_ltv_bps_check;
ALTER TABLE loan_products VALIDATE CONSTRAINT loan_products_interest_method_v2_check;
ALTER TABLE loan_applications ADD COLUMN IF NOT EXISTS require_collateral boolean NOT NULL DEFAULT false;
ALTER TABLE loan_applications ADD COLUMN IF NOT EXISTS tier_snapshot text NOT NULL DEFAULT 'regular';
ALTER TABLE loan_applications ADD COLUMN IF NOT EXISTS rate_discount_bps integer NOT NULL DEFAULT 0;
ALTER TABLE loans ADD COLUMN IF NOT EXISTS tier_snapshot text NOT NULL DEFAULT 'regular';

CREATE TABLE IF NOT EXISTS member_profile_change_requests (
  id bigserial PRIMARY KEY,
  member_id bigint NOT NULL REFERENCES members(id),
  field_name text NOT NULL,
  old_value text NOT NULL DEFAULT '',
  new_value text NOT NULL,
  status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','rejected')),
  reviewed_by bigint REFERENCES users(id),
  review_reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  reviewed_at timestamptz
);
CREATE INDEX IF NOT EXISTS member_profile_change_requests_member_idx ON member_profile_change_requests(member_id,created_at DESC);

CREATE TABLE IF NOT EXISTS member_tier_policies (
  tier text PRIMARY KEY CHECK(tier IN ('regular','platinum','vip')),
  min_qualifying_balance bigint NOT NULL CHECK(min_qualifying_balance>=0),
  max_loan_amount bigint NOT NULL CHECK(max_loan_amount>0),
  loan_discount_bps integer NOT NULL DEFAULT 0 CHECK(loan_discount_bps BETWEEN 0 AND 10000),
  deposit_bonus_bps integer NOT NULL DEFAULT 0 CHECK(deposit_bonus_bps BETWEEN 0 AND 10000),
  priority_label text NOT NULL DEFAULT 'Standar',
  waive_admin_fee boolean NOT NULL DEFAULT false,
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO member_tier_policies(tier,min_qualifying_balance,max_loan_amount,loan_discount_bps,deposit_bonus_bps,priority_label,waive_admin_fee) VALUES
 ('regular',0,25000000,0,0,'Verifikasi standar',false),
 ('platinum',10000000,100000000,300,50,'Prioritas',false),
 ('vip',50000000,500000000,600,100,'Prioritas utama',true)
ON CONFLICT(tier) DO NOTHING;
CREATE TABLE IF NOT EXISTS member_tiers (
  member_id bigint PRIMARY KEY REFERENCES members(id),
  tier text NOT NULL DEFAULT 'regular' REFERENCES member_tier_policies(tier),
  qualifying_balance bigint NOT NULL DEFAULT 0 CHECK(qualifying_balance>=0),
  repayment_current boolean NOT NULL DEFAULT true,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS member_tier_history (
  id bigserial PRIMARY KEY,
  member_id bigint NOT NULL REFERENCES members(id),
  old_tier text,
  new_tier text NOT NULL REFERENCES member_tier_policies(tier),
  qualifying_balance bigint NOT NULL,
  reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO member_tiers(member_id,tier,qualifying_balance,repayment_current)
SELECT id,'regular',0,true FROM members ON CONFLICT(member_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS financial_rate_settings (
  id integer PRIMARY KEY DEFAULT 1 CHECK(id=1),
  savings_interest_bps integer,
  interest_tax_bps integer,
  updated_by bigint REFERENCES users(id),
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO financial_rate_settings(id) VALUES(1) ON CONFLICT(id) DO NOTHING;
CREATE TABLE IF NOT EXISTS deposit_products (
  id bigserial PRIMARY KEY,
  name text NOT NULL,
  tenor_months integer NOT NULL CHECK(tenor_months IN (1,3,6,12,24)),
  annual_rate_bps integer,
  min_amount bigint NOT NULL DEFAULT 1 CHECK(min_amount>0),
  configured boolean NOT NULL DEFAULT false,
  active boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS deposits (
  id bigserial PRIMARY KEY,
  member_id bigint NOT NULL REFERENCES members(id),
  product_id bigint NOT NULL REFERENCES deposit_products(id),
  principal bigint NOT NULL CHECK(principal>0),
  gross_interest bigint NOT NULL DEFAULT 0 CHECK(gross_interest>=0),
  tax_amount bigint NOT NULL DEFAULT 0 CHECK(tax_amount>=0),
  net_interest bigint NOT NULL DEFAULT 0 CHECK(net_interest>=0),
  aro_type text NOT NULL CHECK(aro_type IN ('non_aro','aro_principal','aro_principal_interest')),
  status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','active','matured','closed','rejected')),
  reference text NOT NULL UNIQUE,
  opened_at timestamptz,
  maturity_date date,
  paid_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deposits_member_status_idx ON deposits(member_id,status);
CREATE TABLE IF NOT EXISTS deposit_transactions (
  id bigserial PRIMARY KEY,
  deposit_id bigint NOT NULL REFERENCES deposits(id),
  kind text NOT NULL,
  amount bigint NOT NULL CHECK(amount>=0),
  reference text NOT NULL UNIQUE,
  created_by bigint REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS loan_application_documents (
  application_id bigint NOT NULL REFERENCES loan_applications(id),
  document_id bigint NOT NULL REFERENCES documents(id),
  linked_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(application_id,document_id)
);
CREATE TABLE IF NOT EXISTS loan_collaterals (
  id bigserial PRIMARY KEY,
  application_id bigint NOT NULL REFERENCES loan_applications(id),
  type text NOT NULL CHECK(type IN ('savings','deposit','vehicle','property')),
  details jsonb NOT NULL DEFAULT '{}'::jsonb,
  estimated_value bigint NOT NULL CHECK(estimated_value>0),
  appraised_value bigint,
  status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','rejected','released')),
  review_reason text NOT NULL DEFAULT '',
  reviewed_by bigint REFERENCES users(id),
  reviewed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS collateral_documents (
  collateral_id bigint NOT NULL REFERENCES loan_collaterals(id),
  document_id bigint NOT NULL REFERENCES documents(id),
  PRIMARY KEY(collateral_id,document_id)
);
CREATE TABLE IF NOT EXISTS collateral_locks (
  id bigserial PRIMARY KEY,
  collateral_id bigint NOT NULL REFERENCES loan_collaterals(id),
  member_id bigint NOT NULL REFERENCES members(id),
  asset_type text NOT NULL CHECK(asset_type IN ('savings','deposit')),
  asset_id bigint NOT NULL,
  locked_amount bigint NOT NULL CHECK(locked_amount>0),
  status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','released')),
  created_at timestamptz NOT NULL DEFAULT now(),
  released_at timestamptz
);
CREATE INDEX IF NOT EXISTS collateral_locks_asset_idx ON collateral_locks(asset_type,asset_id) WHERE status='active';

CREATE TABLE IF NOT EXISTS early_repayment_policies (
  id integer PRIMARY KEY DEFAULT 1 CHECK(id=1),
  configured boolean NOT NULL DEFAULT false,
  flat_discount_bps integer NOT NULL DEFAULT 0,
  declining_discount_bps integer NOT NULL DEFAULT 0,
  annuity_discount_bps integer NOT NULL DEFAULT 0,
  fee_bps integer NOT NULL DEFAULT 0,
  updated_by bigint REFERENCES users(id),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK(flat_discount_bps BETWEEN 0 AND 10000),
  CHECK(declining_discount_bps BETWEEN 0 AND 10000),
  CHECK(annuity_discount_bps BETWEEN 0 AND 10000),
  CHECK(fee_bps BETWEEN 0 AND 10000)
);
INSERT INTO early_repayment_policies(id) VALUES(1) ON CONFLICT(id) DO NOTHING;
CREATE TABLE IF NOT EXISTS early_repayments (
  id bigserial PRIMARY KEY,
  loan_id bigint NOT NULL REFERENCES loans(id),
  member_id bigint NOT NULL REFERENCES members(id),
  quoted_amount bigint NOT NULL CHECK(quoted_amount>0),
  status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','rejected','paid')),
  reference text NOT NULL UNIQUE,
  reason text NOT NULL DEFAULT '',
  reviewed_by bigint REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  reviewed_at timestamptz
);

CREATE TABLE IF NOT EXISTS gateway_payment_intents (
  id bigserial PRIMARY KEY,
  user_id bigint NOT NULL REFERENCES users(id),
  member_id bigint REFERENCES members(id),
  provider text NOT NULL,
  provider_reference text NOT NULL,
  target_type text NOT NULL,
  target_id bigint NOT NULL,
  amount bigint NOT NULL CHECK(amount>0),
  currency text NOT NULL DEFAULT 'IDR',
  status text NOT NULL DEFAULT 'pending',
  checkout_url text,
  paid_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(provider,provider_reference)
);
CREATE TABLE IF NOT EXISTS gateway_webhook_events (
  id bigserial PRIMARY KEY,
  provider text NOT NULL,
  event_id text NOT NULL,
  provider_reference text NOT NULL,
  payload_sha256 text NOT NULL,
  processing_status text NOT NULL DEFAULT 'received',
  error_message text NOT NULL DEFAULT '',
  received_at timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz,
  UNIQUE(provider,event_id)
);

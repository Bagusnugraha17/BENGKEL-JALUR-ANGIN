ALTER TABLE users ADD COLUMN IF NOT EXISTS username text;
CREATE UNIQUE INDEX IF NOT EXISTS users_username_ci_unique ON users (LOWER(username)) WHERE username IS NOT NULL;

ALTER TABLE members ADD COLUMN IF NOT EXISTS birth_place text NOT NULL DEFAULT '';
ALTER TABLE members ADD COLUMN IF NOT EXISTS gender text NOT NULL DEFAULT 'unspecified';
ALTER TABLE members ADD COLUMN IF NOT EXISTS city_regency text NOT NULL DEFAULT '';
ALTER TABLE members ADD COLUMN IF NOT EXISTS postal_code text NOT NULL DEFAULT '';
ALTER TABLE members ADD COLUMN IF NOT EXISTS monthly_income bigint NOT NULL DEFAULT 0 CHECK (monthly_income >= 0);
ALTER TABLE members ADD COLUMN IF NOT EXISTS coop_unit text NOT NULL DEFAULT '';
ALTER TABLE members ADD COLUMN IF NOT EXISTS referral_code text NOT NULL DEFAULT '';
ALTER TABLE members ADD COLUMN IF NOT EXISTS membership_purpose text NOT NULL DEFAULT '';
ALTER TABLE members ADD COLUMN IF NOT EXISTS additional_notes text NOT NULL DEFAULT '';
ALTER TABLE members ADD COLUMN IF NOT EXISTS transaction_pin_hash text NOT NULL DEFAULT '';

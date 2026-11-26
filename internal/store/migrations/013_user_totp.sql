-- Add RFC 6238 TOTP two-factor authentication columns to users.
ALTER TABLE users ADD COLUMN totp_secret TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN totp_recovery_hashes TEXT NOT NULL DEFAULT '';

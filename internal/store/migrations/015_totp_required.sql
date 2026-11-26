-- Allow admins to enforce TOTP two-factor authentication on users and invite links.
ALTER TABLE users ADD COLUMN totp_required INTEGER NOT NULL DEFAULT 0;
ALTER TABLE user_invites ADD COLUMN require_totp INTEGER NOT NULL DEFAULT 0;

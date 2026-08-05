-- 0012_create_feishu_oauth_tokens.sql (PostgreSQL)
--
-- Rotating delegated user_access_token state for the optional Feishu external
-- group image reader. Both token columns contain AES-GCM ciphertext; raw OAuth
-- credentials must never be written to the database.

CREATE TABLE IF NOT EXISTS feishu_oauth_tokens (
    purpose                       VARCHAR(64)  PRIMARY KEY,
    access_token_ciphertext       TEXT         NOT NULL,
    refresh_token_ciphertext      TEXT         NOT NULL,
    access_expires_at             TIMESTAMPTZ  NOT NULL,
    refresh_token_expires_at      TIMESTAMPTZ,
    updated_at                    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

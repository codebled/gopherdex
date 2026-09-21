-- Which optional emails a user wants. Security emails (password, 2FA,
-- tokens, trusted publishers, new email address) are always sent.
ALTER TABLE users ADD COLUMN notify_publish INTEGER NOT NULL DEFAULT 1; -- a version of my module was published
ALTER TABLE users ADD COLUMN notify_access INTEGER NOT NULL DEFAULT 1;  -- I was given access to a module or organization

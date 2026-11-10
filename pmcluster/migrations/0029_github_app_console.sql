-- 0029_github_app_console.sql — the registration facts the console's set/rotate
-- form has to show, which the row did not carry.
--
-- key_fingerprint is SHA-256 of the key's public half, rendered like
-- `ssh-keygen -lf`: it answers "which key is installed" without the key ever
-- travelling back to a browser, and it is what makes a rotation observable
-- (same key = same fingerprint, another key = another one).
--
-- last_probe_* record the last time a registration change or the Test button
-- actually minted a token and read the configured targets. Without them the
-- console can only say "a key is stored" — which is not the same claim as "the
-- pairing worked five minutes ago", and the operator overwriting a working key
-- needs the difference. last_probe_at is a Unix timestamp, 0 = never probed.
ALTER TABLE github_app ADD COLUMN key_fingerprint   TEXT    NOT NULL DEFAULT '';
ALTER TABLE github_app ADD COLUMN last_probe_ok     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE github_app ADD COLUMN last_probe_detail TEXT    NOT NULL DEFAULT '';
ALTER TABLE github_app ADD COLUMN last_probe_at     INTEGER NOT NULL DEFAULT 0;

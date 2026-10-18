CREATE TABLE investigation_reservations (
 id TEXT PRIMARY KEY,
 bucket TEXT NOT NULL,
 reserved INTEGER NOT NULL CHECK(reserved > 0),
 charged INTEGER NOT NULL DEFAULT 0 CHECK(charged >= 0),
 status TEXT NOT NULL CHECK(status IN ('reserved','dispatched','settled')),
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX investigation_bucket ON investigation_reservations(bucket);

-- Storage-failover marker (v0.2.159).
--
-- When the control loop automatically relocates a stateful stack off a failed
-- storage node (S3 restore), it records the move here so the console, CLI and
-- public badge can surface it: the app is running on a new node from the latest
-- offsite backup, which may be missing writes made after that backup. The
-- operator either acknowledges (mark healthy on the new node) or moves the
-- stack back once the original node recovers. Cleared on move-back.
CREATE TABLE stack_failover (
    stack_name TEXT PRIMARY KEY,
    from_node  TEXT NOT NULL,
    to_node    TEXT NOT NULL,
    at         INTEGER NOT NULL,
    acked      INTEGER NOT NULL DEFAULT 0
) STRICT;

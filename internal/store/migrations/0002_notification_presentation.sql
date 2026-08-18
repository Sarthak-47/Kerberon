-- A dispatch worker must be able to render a page from the outbox row alone.
--
-- Without these, delivery had only a body and a destination: ntfy could not
-- set a priority (so every page looked critical), Telegram could not render
-- its acknowledge button, and email had no subject. Reading the incident at
-- send time would work but puts a join on the hot path of the one operation
-- that must not be slow, and would reflect the incident's state now rather
-- than when the page was composed.

ALTER TABLE notifications ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE notifications ADD COLUMN severity TEXT NOT NULL DEFAULT '';
ALTER TABLE notifications ADD COLUMN ack_url TEXT NOT NULL DEFAULT '';

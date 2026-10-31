-- Times are RFC 3339 in UTC, such as 2026-10-29T18:30:00Z, so that comparing
-- them as text compares them as times.

-- Meet-ups, as Meetup lists them: SyncEvents keeps them in step.
CREATE TABLE IF NOT EXISTS events (
	id        INTEGER PRIMARY KEY,
	meetup_id TEXT NOT NULL UNIQUE,
	title     TEXT NOT NULL,
	url       TEXT NOT NULL,
	starts_at TEXT NOT NULL,
	ends_at   TEXT NOT NULL,
	CHECK (ends_at > starts_at)
);

CREATE INDEX IF NOT EXISTS events_ends_at ON events (ends_at);

-- An event's cover image on Meetup, for the home page: SyncCover saves it.
-- source is the image's address on Meetup, so that it is downloaded again
-- only when the cover changes. A table of its own, rather than columns on
-- events, so that it is created in a database that already has events.
CREATE TABLE IF NOT EXISTS event_covers (
	event_id INTEGER PRIMARY KEY REFERENCES events (id) ON DELETE CASCADE,
	source   TEXT NOT NULL,
	image    BLOB NOT NULL
);

-- An event's raffle. Entries are deleted a week after the event ends, by
-- Prune; drawn_at is set when an entry wins, so it is not drawn again.
CREATE TABLE IF NOT EXISTS raffle_entries (
	id       INTEGER PRIMARY KEY,
	event_id INTEGER NOT NULL REFERENCES events (id) ON DELETE CASCADE,
	name     TEXT NOT NULL,
	email    TEXT NOT NULL,
	drawn_at TEXT,
	UNIQUE (event_id, email)
);

CREATE TABLE IF NOT EXISTS talk_proposals (
	id               INTEGER PRIMARY KEY,
	name             TEXT NOT NULL,
	email            TEXT NOT NULL,
	title            TEXT NOT NULL,
	description      TEXT NOT NULL,
	duration_minutes INTEGER NOT NULL,
	remote           INTEGER NOT NULL,
	submitted_at     TEXT NOT NULL
);

-- Organisers' sessions. The token itself is only ever in the browser's
-- cookie: a copy of this table cannot be used to sign in.
CREATE TABLE IF NOT EXISTS sessions (
	token_hash BLOB PRIMARY KEY,
	login      TEXT NOT NULL,
	expires_at TEXT NOT NULL
);

-- Talks on the YouTube channel, as its feed lists them: SaveVideos adds
-- them, and keeps them once the feed, which lists only the latest, does not.
CREATE TABLE IF NOT EXISTS videos (
	youtube_id   TEXT PRIMARY KEY,
	title        TEXT NOT NULL,
	published_at TEXT NOT NULL
);

-- Each event's raffle code, which organisers show at the meet-up and
-- entrants type in, so that only those in the room can enter. A table of its
-- own, as event_covers is, so that it is created in an existing database.
CREATE TABLE IF NOT EXISTS raffle_codes (
	event_id INTEGER PRIMARY KEY REFERENCES events (id) ON DELETE CASCADE,
	code     TEXT NOT NULL
);

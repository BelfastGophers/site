# belfastgophers.com

The Belfast Gophers website: the static pages, a talk proposal form, the
meet-up raffle, and pages for the organisers. Generated with
[httpgen](https://github.com/uRadical/httpgen), whose files under `api/rest/`
are managed by it: `go tool httpgen sync` keeps them up to date.

## Running it

```
GITHUB_CLIENT_ID=... GITHUB_CLIENT_SECRET=... make run
```

| Setting | Default | |
|---|---|---|
| `-db` / `DB` | `site.db` | the SQLite database, created if it does not exist |
| `-base-url` / `BASE_URL` | `http://localhost:8080` | where the site is reached: `https://belfastgophers.com` in production |
| `-github-org` / `GITHUB_ORG` | `BelfastGophers` | whose members are organisers |
| `-github-client-id` / `GITHUB_CLIENT_ID` | | the GitHub App's client id |
| `GITHUB_CLIENT_SECRET` | | its secret, from the environment only |
| `-meetup-feed` / `MEETUP_FEED` | the group's feed | the Meetup iCalendar feed events come from |

`go run ./cmd/site -help` lists the rest.

For development, [air](https://github.com/air-verse/air) rebuilds and restarts
the site whenever its Go code, templates or static files change:

```
go install github.com/air-verse/air@latest
GITHUB_CLIENT_ID=... GITHUB_CLIENT_SECRET=... air
```

It reads `.air.toml`, builds into `tmp/`, and takes the same environment as
`make run`. Refresh the browser to see a change.

## Organisers

Organisers sign in with GitHub; anyone in the GitHub organisation is one. They
draw each event's raffle and read talk proposals.

Signing in uses the BelfastGophers GitHub App, owned by and installed on the
organisation, with the organisation permission **Members: Read-only**, which
the membership check needs. Its callback URLs are `<base url>/auth/callback`
for each place the site runs, `http://localhost:8080/auth/callback` locally.
The site keeps nothing of GitHub's after sign-in.

Membership is checked only at sign-in, so someone taken out of the
organisation stays signed in until their session ends, at most four hours
later. To end it now, delete their sessions on the server, by their GitHub
login:

```
sudo sqlite3 /var/lib/site/site.db "DELETE FROM sessions WHERE login = 'their-login'"
```

## Data

Events come from [Meetup](https://www.meetup.com/belfast-gophers/): every hour
the site reads the group's public iCalendar feed, which needs no API key, adds
and updates events, and deletes any yet to start that Meetup no longer lists.
If the feed cannot be read, the events stay as they were. The home page shows
the next one, with a link to sign up on Meetup.

Events drive the rest. The raffle is open while an event is under way, and its
entries are deleted a week after it ends. The proposal form offers the
upcoming events as dates. Proposals are kept.

The site's cookies are always `Secure`. Browsers count `localhost`, and
names under it such as `belfastgophers.localhost`, as secure over plain HTTP,
so signing in works locally at `http://belfastgophers.localhost:8080` with
`BASE_URL` set to that.

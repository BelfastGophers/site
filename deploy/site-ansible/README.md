# Belfast Gophers web server

Takes a fresh Fedora server to a running site: admin user with sudo, key-only
SSH with root disabled, LLMNR/mDNS off, the `gophers` deploy group, the `site`
service user, `/opt/site` release layout with SELinux labels, the restricted
`deploy` user for GitHub Actions, root-only secrets in `/etc/site/site.env`,
and the systemd unit serving HTTPS on 443 (HTTP on 80 redirects) with
Let's Encrypt certificates, the database and the certificate cache all in
`/var/lib/site`.

## Layout

```
site.yml                     playbook (asserts you can't lock yourself out)
inventory.ini                server address
group_vars/all/vars.yml      settings
group_vars/all/vault.yml     secrets (you create this, encrypted)
files/gh_deploy.pub          CI public key (optional, you add it)
files/motd-logo.png          logo shown at SSH login
roles/base                   hostname, packages, viu, login logo, resolved,
                             admin user, sshd
roles/app                    users, dirs, SELinux, deploy script, sudoers, unit
roles/updates                daily automatic updates, reboot-required marker
roles/logs                   Vector, shipping the logs to Better Stack
```

## One-off setup on your laptop

```bash
pip install ansible            # includes ansible.posix and community.general
# or, with ansible-core only:
ansible-galaxy collection install -r requirements.yml

ansible-vault create group_vars/all/vault.yml   # paste vault.yml.example, fill in
openssl passwd -6                               # sudo hash for each admin
```

Set the server IP in `inventory.ini`. For CI deploys, copy the public half of
the GitHub Actions key to `files/gh_deploy.pub`.

## Fresh server

Create the server in Hetzner with an SSH key for root. Point DNS at it
before the first run: A and AAAA records for both `belfastgophers.com` and
`www`, using the single address ending `::1` from the server's IPv6 `/64`.
Let's Encrypt validates on the first HTTPS request, and failed attempts count
against its rate limits. To bring a server up before DNS is ready, run with
`-e site_domain= -e site_port=80 -e site_base_url=http://SERVER_IP` and run
again without them once DNS resolves. Then build the binary
and run the playbook as root, once:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o site ./cmd/site

ansible-playbook site.yml -u root --ask-vault-pass \
  -e site_binary_src=/absolute/path/to/site
```

`site_binary_src` is only used when the server has no release yet; after that,
releases go through the deploy script. sshd is restarted at the very end, so
the run completes as root, and every later run is as your admin user:

```bash
ansible-playbook site.yml -u alan --ask-become-pass --ask-vault-pass
```

With the YubiKey key, `ControlPersist` in `ansible.cfg` keeps one connection
open, so you should only need to touch the sensor once per run.

## Existing server

Safe to run against the current server. It converges what's already there:
`current` exists, so no binary is uploaded; your sudo password is only set on
user creation; `site.env` is rewritten from the vault, so make sure every
variable the app needs is in `vault_site_env`. Old releases are left alone.

## HTTPS

The binary handles TLS itself, so there is no reverse proxy to maintain.
`site_domain`, `site_port`, `site_http_port` and `site_base_url` in
`vars.yml` become flags in the unit, and the playbook checks they agree.
Certificates are cached in `/var/lib/site/certs`, which survives deploys and
restarts, and renew automatically. Secrets stay in `/etc/site/site.env`;
everything else is visible with `systemctl cat site`.

The GitHub App's callback URL must use `site_base_url`.

## Logs

Vector reads the journals of the site and of automatic updates (see
Updates), and sends each line to Better Stack. A site line becomes an event
with its JSON fields: `level`, `msg`, `request_id`, status,
route and the rest. The site's logs stay in the journal too, for
`journalctl -u site`.

1. In Better Stack, create a log source with Vector as the platform.
2. Put its ingesting host in `betterstack_ingest_host` in `vars.yml`.
3. Put its token in the vault as `vault_betterstack_token`
   (`ansible-vault edit group_vars/all/vault.yml`).
4. Run the playbook.

Until both are set, the role is skipped. The token is a Vector secret, in
`/etc/vector/secrets/betterstack_token`, readable only by Vector; the config
in `/etc/vector/vector.yaml` refers to it as `SECRET[local.betterstack_token]`,
and is checked with `vector validate` before it replaces the live one. Vector
does not expand environment variables in its config by default, so the token
cannot be passed that way. To see it working, run
`journalctl -u vector -f` on the server and load a page: the request appears
in Better Stack's live tail.

## Updates

dnf5 automatic applies updates every day, at 06:00 UTC plus up to an hour
(`systemctl list-timers dnf5-automatic.timer`; its output is in
`journalctl -u dnf5-automatic` and in Better Stack). It never reboots. When
the updates include a package that needs a reboot, such as the kernel, it
runs `/usr/local/sbin/reboot-required` instead, which:

- writes `/run/reboot-required` (also `/var/run/reboot-required`): when,
  the running kernel and the newest installed one;
- adds a line to the login message, for everyone who logs in;
- logs `reboot required: ...`, which Vector sends to Better Stack.

While `/run/reboot-required` exists, `reboot-reminder.timer` logs
`reboot still required: ...` every day at 09:00 UTC. `/run` is cleared at
boot, so rebooting clears all of it: reboot when it suits, with
`sudo systemctl reboot`.

Every event has a `unit` field: `site.service` for the site,
`dnf5-automatic.service` or `reboot-reminder.service` for updates.

### The reboot alert

The alert is set up in Better Stack, not here. In Telemetry, Explore logs,
on the site's source:

1. Under Log filtering, deselect the preset, and filter with
   `/"msg":"reboot (still )?required/`. It matches both lines, and never a
   site request, whose `msg` is `request`.
2. Add an alert with the Threshold detection method, alerting when any
   series is higher than `0`.
3. Under Advanced settings, choose who to alert and how (E-mail), and set
   "on data from" to the last 5 minutes.

The window matters. One reboot line is a single event, and with a window of
the last minute it is counted by one check only: the next check's count is
back to 0, so the 1-minute confirmation period is never met and no incident
opens. With 5 minutes, five checks in a row count it.

The incident resolves itself once the window passes the last line. While a
reboot is owed, the daily reminder opens a new one, so the alert repeats
until you reboot.

To test it without an update:

```bash
sudo /usr/local/sbin/reboot-required           # marker and login message
sudo systemctl start reboot-reminder.service   # logs to Better Stack
sudo rm /run/reboot-required /run/motd.d/reboot-required   # undo
```

The script's own log line is only sent when dnf5 automatic runs it; run by
hand, the reminder is what reaches Better Stack.

## Login logo

Everyone who logs in over SSH sees the Belfast Gophers logo, then
`motd_message`, then any other login message (such as a pending reboot).
The base role installs [viu](https://github.com/atanunq/viu) from its GitHub
release (Fedora doesn't package it; the version and digests are in
`vars.yml`), has it draw `files/motd-logo.png` as coloured text,
`motd_logo_width` columns by `motd_logo_height` lines, and writes the result
to `/etc/motd.d/00-logo`, which pam_motd prints. It is drawn once, on the
server, rather than at every login, so it costs nothing at login and shows
the same in every terminal. It uses 24-bit colour, which most terminals
support (macOS Terminal before Tahoe does not). To change the logo or its
size, change `motd_logo_src`, `motd_logo_width` or `motd_logo_height` and
run the playbook. viu is also there to use by hand: `viu image.png`.

## People

Everyone with a login is listed under `users` in `vars.yml`, and their public
keys are in `files/keys/<name>.pub`. To add someone:

1. Put their public key(s) in `files/keys/<name>.pub`, one per line.
2. Add them to `users` with `deployer: true` and, for full sudo, `admin: true`.
3. For admins, add their password hash to `vault_user_password_hashes`
   (`ansible-vault edit group_vars/all/vault.yml`).
4. Run the playbook.

To revoke a key, delete its line from the `.pub` file. To remove a person,
delete them from `users`, add them to `removed_users`, and run the playbook.
Group membership is managed exactly, so changing `admin` or `deployer` adds
or removes access on the next run.

## Deploying

```bash
# CI (deploy user, forced command)
ssh deploy@SERVER deploy < site

# You (as alan, member of gophers)
ssh SERVER 'SSH_ORIGINAL_COMMAND=deploy /usr/local/bin/site-deploy' < site
```

The script rejects empty or non-ELF uploads, swaps `current` atomically,
restarts the service, and keeps the five newest releases (never deleting the
live one). Roll back by pointing `current` at an older release and running
`sudo systemctl restart site`.

## Not covered

- **Hetzner Cloud Firewall**: create it in the console (TCP 22, 80, 443), or
  automate it with the `hetzner.hcloud` collection.
- **DNS**: A and AAAA records at Namecheap for the domain and `www`.
- **Backups**: `sqlite3 /var/lib/site/site.db ".backup <path>"` on a timer,
  copied off the server.

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
roles/base                   hostname, packages, resolved, admin user, sshd
roles/app                    users, dirs, SELinux, deploy script, sudoers, unit
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

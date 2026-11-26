# Running on a server

One Linux VM runs gitsynth, with [Caddy](https://caddyserver.com) in front for TLS. [deploy/](../deploy) holds the systemd unit and the Caddyfile. The examples use the domain `gitsynth.example.com` and a first user `alice`.

## What it needs

- A Linux VM with systemd and a local disk (not network storage such as NFS). Roughly 2 vCPUs, 2 GB of memory and 30–60 GB of disk.
- git 2.38 or later.
- A domain whose A record (and AAAA, with IPv6) points at the VM.
- TCP 80 and 443 open to the VM, in the provider's firewall too. Caddy uses 80 to get the certificate and to redirect to HTTPS.
- For git over SSH (optional), TCP 22 or another port open to the VM. See [Git over SSH](#git-over-ssh).

## Installing

Take the release archive for the VM's architecture and check it against the release's `SHA256SUMS`, or build one yourself (see [CONTRIBUTING.md](../CONTRIBUTING.md)).

```bash
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf gitsynth-v0.1.0-linux-amd64.tar.gz
cd gitsynth-v0.1.0-linux-amd64
sudo install -m 755 gitsynth /usr/local/bin/gitsynth
```

The server runs as a user of its own, which must own the data directory: it runs git with that user's git config ignored, so `safe.directory` cannot excuse repositories owned by anyone else.

```bash
sudo useradd --system --user-group --home-dir /var/lib/gitsynth --shell /usr/sbin/nologin gitsynth
sudo install -d -o gitsynth -g gitsynth -m 750 /var/lib/gitsynth
sudo -u gitsynth /usr/local/bin/gitsynth admin init --data /var/lib/gitsynth --user alice --host gitsynth.example.com
```

The host cannot change later. Every identity is qualified with it (`user:alice@gitsynth.example.com`), and it fixes the server's public URL, `https://gitsynth.example.com`. `init` prints a token, shown only once, and a passkey link that lasts 24 hours.

Install the unit, which runs the server on `127.0.0.1:3000`, then Caddy as its documentation describes for your distribution, with your domain in the Caddyfile:

```bash
sudo install -m 644 deploy/gitsynth.service /etc/systemd/system/gitsynth.service
sudo systemctl enable --now gitsynth
sudo install -m 644 deploy/Caddyfile /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

Open the passkey link and create a passkey. To clone and push over HTTPS, issue a token on the Identities page; for SSH, see below. A push is refused unless each new commit's committer is you: commit with `GIT_COMMITTER_EMAIL=user:alice@gitsynth.example.com`, or register your own address on the Identities page.

## The public URL

Clone URLs, the links `admin` prints, and passkeys all start from the public URL. It is `https://HOST` for the host given to `init`, and the server never takes it from a request's `Host` or `X-Forwarded-*` headers. When people reach the server somewhere else, such as another port, give `serve` and `admin` the same `--url`.

## Git over SSH

`serve --ssh ADDR` serves git over SSH as well, to keys registered on the Identities page. It runs `git-upload-pack` and `git-receive-pack` and nothing else: no shell, no forwarding. Caddy does not carry SSH, and neither does an HTTP proxy such as Cloudflare's, so the name clients connect to must point at the VM itself.

Port 22 is usually taken by the VM's own sshd. Pick one:

- **gitsynth on 22**, for clone URLs like `git@gitsynth.example.com:acme/app.git`. Move the VM's sshd out of the way first, and make sure you can still reach the VM another way (a Tailscale address, the provider's console). With Tailscale SSH, tailscaled answers port 22 on the tailnet address itself, so the OS sshd can stop listening on the public address. If something else keeps port 22 on another address, give `--ssh` the public address alone, such as `--ssh 203.0.113.10:22`.
- **gitsynth on another port**, such as `--ssh :2222`, for clone URLs like `ssh://git@gitsynth.example.com:2222/acme/app.git`. sshd stays as it is.

Add the flag in a drop-in, so upgrades of the unit file keep it. A port below 1024 needs `CAP_NET_BIND_SERVICE`; leave those two lines out for a higher port:

```bash
sudo systemctl edit gitsynth
```

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/gitsynth serve --data /var/lib/gitsynth --listen 127.0.0.1:3000 --ssh :22
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
```

Then `sudo systemctl restart gitsynth`, and open the port in the provider's firewall.

The host key is made on first start, as `ssh_host_ed25519_key` in the data directory, so backups and moves keep it. The server logs its fingerprint when it starts; give it to people to check against what ssh shows on their first connection:

```bash
sudo journalctl -u gitsynth | grep host_key
```

Clone URLs use the `--url` host on the `--ssh` port (left out when it is 22). When clients reach SSH under another name or port, such as through a port mapping, say so with `--ssh-host`, such as `--ssh-host git.example.com:2222`.

To check a key, `ssh -T git@gitsynth.example.com` answers with the identity it belongs to. [docs/agents.md](agents.md#git-over-ssh) says how to clone for each identity.

## Admin commands

Run them as the gitsynth user, with the data directory. `gitsynth` with no arguments lists them. For example, a one-time sign-in link for when a passkey is lost:

```bash
sudo -u gitsynth /usr/local/bin/gitsynth admin login-link --data /var/lib/gitsynth --user alice
```

## Upgrading

Take a backup first: there are no versioned schema migrations yet. Then install the new binary and restart:

```bash
sudo install -m 755 gitsynth /usr/local/bin/gitsynth
sudo systemctl restart gitsynth
```

On start, the server points every repository's hook at itself and logs its version, which `gitsynth version` also prints.

## Backups

There is no online backup yet. A copy taken while the server runs can catch the databases and repositories halfway through a write, so stop it first:

```bash
sudo systemctl stop gitsynth
sudo tar -C /var/lib -czf gitsynth-backup.tar.gz gitsynth
sudo systemctl start gitsynth
```

A backup can be restored anywhere: start the server with `--data` pointing at it.

## Bringing in an existing repository

Create the repository in the web UI, then push its history as a repository admin. Commits by committers the server does not know are accepted and recorded as foreign.

```bash
git -c "http.extraHeader=Authorization: Bearer $TOKEN" push --tags https://gitsynth.example.com/git/acme/app.git HEAD:main
```

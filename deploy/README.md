# Deploying

The API is one container with one SQLite file. It runs anywhere Docker does;
these notes assume a home server behind a Cloudflare tunnel, which is the
current setup.

## First run

1. **Create the tunnel.** In Cloudflare Zero Trust → Networks → Tunnels, create
   a tunnel and add a public hostname `api.fetchandscore.com` routed to
   `http://api:8080`. Copy the tunnel token.

2. **Configure.** `cp .env.example .env` and fill in the tunnel token and the
   Mailgun credentials.

3. **Set up Mailgun DNS.** Add the SPF and DKIM records Mailgun gives you for
   the sending domain. Sign-in links land in spam without them.

4. **Start it.**
   ```sh
   docker compose up -d
   docker compose logs -f api
   ```

5. **Create a club and invite yourself.**
   ```sh
   docker compose exec api /usr/local/bin/fnsctl invite \
     -club demo -email you@example.com -role admin
   ```

## Why no published ports

`cloudflared` dials out to Cloudflare and traffic is routed back down that
connection. There is no inbound port, no port forwarding, and no public IP
exposed. The API container is not reachable from the host network at all —
only the tunnel can talk to it.

## Backups

The backup container runs `VACUUM INTO` daily and keeps a fortnight of
snapshots in `./backups`. It uses `VACUUM INTO` rather than copying the file
because SQLite in WAL mode is frequently mid-write, and a copied file can be
torn. Copy `./backups` somewhere off the machine; a snapshot on the same disk
is not a backup.

To restore, stop the stack, replace `./data/fetchandscore.db` with a snapshot,
and start it again.

## Moving to a VPS

Nothing here is specific to a home server. On a VPS you can either keep the
tunnel exactly as is, or drop the `tunnel` service, publish port 8080, and put
Caddy in front for automatic TLS. The API reads its whole configuration from
the environment, so no rebuild is involved either way.

## Why not Lambda

SQLite needs a persistent disk, and the live scoring feed needs a long-lived
connection. Lambda offers neither. Running there would mean replacing both the
database and the realtime layer — a different application, not a redeployment.

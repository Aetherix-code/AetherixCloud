# Cluster bootstrap (VPS)

Every machine runs the same daemon, `aetherixd`. There is no separate control plane: cluster state replicates through Corrosion (or a single-node SQLite fallback), and nodes mesh over WireGuard.

## One-line install

The join token RPC embeds this installer URL (`internal/api/machine_svc.go`):

```sh
# First machine — create the cluster
curl -fsSL https://raw.githubusercontent.com/Aetherix-code/AetherixCloud/main/deploy/install.sh | sudo sh -s -- --init --advertise YOUR_PUBLIC_IP

# Installs Docker, Corrosion, Caddy, BuildKit, and Railpack (Dockerfile-less
# GitHub builds). Pass --skip-builder to omit BuildKit/Railpack.

# Additional machines — token from the dashboard or CLI (valid ~1 hour)
curl -fsSL https://raw.githubusercontent.com/Aetherix-code/AetherixCloud/main/deploy/install.sh | sudo sh -s -- --join 'axjoin1.…'
```

From a checkout (no release hosting yet):

```sh
sudo sh deploy/install.sh --init --advertise YOUR_PUBLIC_IP --version v0.0.0
# or build locally and copy bin/aetherixd + bin/ax to /usr/local/bin, then:
sudo aetherixd init --advertise YOUR_PUBLIC_IP
sudo systemctl enable --now aetherixd
```

`install.sh` installs Docker (with mesh `insecure-registries`), `aetherixd`/`ax`, Corrosion, Caddy, BuildKit, Railpack, and systemd units. Later **dashboard upgrades** refresh `aetherixd`/`ax` and re-sync host tooling (BuildKit/Railpack versions, units, env) without re-running install.

## CLI remote bootstrap

If `aetherixd` is already on the host:

```sh
ax machine init user@host --advertise HOST_PUBLIC_IP
ax machine add user@host                 # mint token + join over SSH
ax machine add --print                   # print token + install command only
```

## After init

1. Save the printed **admin token** (shown once):
   ```sh
   ax context add default --url https://YOUR_IP:8443 --token TOKEN --insecure
   ```
2. Open `https://YOUR_IP:8443` (API + embedded dashboard).
3. With ingress/Caddy up, apps get default hostnames like `<service>-<project>.<ip>.sslip.io`. Set a cluster domain in Settings when you have one.

## Ports

| Port | Purpose |
| --- | --- |
| TCP 80 / 443 | Caddy (HTTP + HTTPS ingress) |
| TCP 8443 | Public TLS API / dashboard |
| UDP 51820 | WireGuard mesh |

## Join flow (what the token carries)

Join tokens (`axjoin1.…`) embed the cluster CA, API endpoints of existing nodes, and a one-time secret (TTL default 1 hour). `aetherixd join` pins TLS to that CA, registers the machine, receives a `/24` from `10.210.0.0/16`, and starts syncing the store.

## Systemd

Unit: `deploy/aetherixd.service` → `/etc/systemd/system/aetherixd.service`.

```sh
systemctl status aetherixd
journalctl -u aetherixd -f
```

Optional environment (drop-in under `aetherixd.service.d/`): `AX_ACME_EMAIL`, `AX_DNS_CLOUDFLARE_TOKEN`, `AX_CADDY_BIN`, `AX_INGRESS=off`, `AX_RESCHEDULE_GRACE`.

## Disaster recovery (cluster snapshot)

While the cluster is healthy, download a store snapshot (projects, services, secrets, users, … plus cluster CA and cluster key):

```sh
ax cluster backup -o cluster.snapshot
# or Settings → Backups → Download snapshot
```

**Not included:** Docker volume contents on disk. For volumes marked `backup-restore`, use the S3 volume backup settings on the same page.

### After total loss

1. Provision a new first machine and install `aetherixd` (or use `install.sh`).
2. Initialise from the snapshot (recovers identity; applies store on first start):

   ```sh
   sudo aetherixd init --from-backup /path/to/cluster.snapshot --advertise YOUR_PUBLIC_IP
   # or: sudo sh deploy/install.sh --init --from-backup /path/to/cluster.snapshot --advertise YOUR_PUBLIC_IP
   # or: ax machine init --from-backup /path/to/cluster.snapshot
   sudo systemctl enable --now aetherixd
   ```

3. Save the printed **admin token** and open the dashboard / `ax context add`.
4. **Rejoin** other machines (`ax machine add` / install `--join`). Snapshot peer machine rows and replicas are cleared on restore — old nodes must join again with new WireGuard keys.
5. **Volumes:** restore from S3 (`Settings → Backups` volume restore, or the node backup API) for any `backup-restore` volumes.

### Restore into a running node

`ax cluster restore FILE` / Settings → Backups → Upload & restore calls `ClusterService.Restore`.

**Safety:** refused unless the store looks freshly initialised (≤1 machine, no projects/services), unless you pass `--force` / confirm **destroy cluster state** in the UI. That path replaces store tables, drops peer machines and all replicas, and keeps the recovering node as the sole member. Prefer `init --from-backup` after total loss so CA + cluster key come from the snapshot.

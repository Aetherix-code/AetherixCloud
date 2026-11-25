# GitHub App setup

AetherixCloud builds and deploys on push through a GitHub App. Webhooks hit any node; a machine with the `builder` role claims the build lease.

## From the dashboard

1. Open **Settings → GitHub** (route `/settings/github`).
2. Start the **manifest flow**. The daemon builds an App manifest (`internal/github/manifest.go`) with:
   - Webhook URL: `https://<dashboard>/api/github/webhook`
   - Callback: `https://<dashboard>/api/github/manifest/callback`
   - Permissions: contents (read), metadata, pull_requests / statuses / checks (write)
   - Events: `push`, `pull_request`
3. Continue to GitHub, create the App, and return through the callback. Credentials (app ID, private key, webhook secret) are stored encrypted in the cluster store.
4. Install the App on the org/repos you want to deploy.

## Local playground without a public URL

- `ax dev tunnel` — expose the webhook with a quick tunnel and refresh the App webhook URL, or
- Dashboard **Simulate push** / `ax dev push <service>` — fetch the commit via the GitHub API and run the real build pipeline without inbound webhooks.

## Build pipeline (what runs on push)

Detection order (`internal/build`):

1. Explicit service setting (`dockerfile` | `railpack` | `image`)
2. Dockerfile at the configured path
3. Compose `build:` sections
4. **Railpack** (stack detect → BuildKit frontend)

Images land in the node’s embedded registry (`127.0.0.1:5000` / mesh `:5000`), then a deployment is created. Commit statuses/checks report progress. Optional PR preview hostnames: `pr-<n>-<service>.<domain>`.

## CLI without GitHub

```sh
ax up                  # upload a local directory to a builder and deploy
ax build --local       # build on the laptop (see ax build --help)
ax deploy -f compose.yaml
```

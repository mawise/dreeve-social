# 🏅 dreeve-social

A lightweight, single-binary Go sidecar for [Dreeve](https://github.com/dreeveapp/dreeve) that adds lightweight social features—workout emails, weekly digests, personal RSS feeds, and 1-click Kudos with instant push notifications—without relying on Strava or forcing your friends to create accounts.

Built with Go's standard library and pure-Go SQLite (`modernc.org/sqlite`), the entire container image is **~18 MB** and idles at **~15 MB of RAM**.

---

## ✨ Features

* **Zero-Account Sharing for Friends:** Friends receive clean HTML emails (via Mailgun) or subscribe to their own private RSS feed without ever creating a login or installing an app.
* **Per-Activity or Weekly Digest Modes:** Each friend can choose between **Instant (Per-Activity)** emails, a **Weekly Digest** summary email, or **Unsubscribed**.
* **Self-Service Preferences & Re-Subscribe:** Every email and RSS feed includes an HMAC-signed `/preferences` link. Friends can switch frequency, unsubscribe, or re-subscribe at any time using the same link.
* **Personal RSS Feeds (Instant & Weekly):** Generates per-friend HMAC-signed RSS 2.0 feeds (`/feed.xml`) in both `instant` and `weekly` modes, with each friend's personal 1-click Kudos button embedded directly in every feed item.
* **Scanner-Proof 1-Click Kudos:** Clicking **"👍 Give Kudos"** opens a lightweight confirmation page that submits the kudos via a client-side `POST` request (with a `<noscript>` button fallback), preventing corporate email link scanners (Outlook SafeLinks, Gmail proxies) from triggering false kudos.
* **Instant Kudos Push Alerts:** Sends a real-time push notification to your phone via [ntfy](https://ntfy.sh) whenever a friend cheers your workout.
* **Automatic GPX Stats Calculation:** Polls `GET /api/v1/activities` on Dreeve and automatically falls back to parsing `GET /api/v1/activities/{id}/gpx` to compute Haversine distance (`km` / `mi`), duration, and elevation gain (`m`) when needed.
* **Private Dreeve Instance Friendly:** Communicates with Dreeve over your internal Docker network (`http://dreeve:8080`), so your Dreeve instance never has to be exposed to the public internet.

---

## 🚀 Quick Start (Docker Compose)

Add `dreeve-social` alongside your existing Dreeve service in `docker-compose.yml`:

```yaml
services:
  dreeve-social:
    image: ghcr.io/mawise/dreeve-social:latest
    container_name: dreeve-social
    restart: unless-stopped
    mem_limit: 128m
    volumes:
      - ./social-data:/data
    environment:
      - TZ=America/Chicago
      - DB_PATH=/data/sidecar.db
      - ATHLETE_NAME=YourName
      - DREEVE_URL=http://dreeve:8080
      - DREEVE_API_TOKEN=drv_replace_with_your_64_hex_char_token
      - PUBLIC_BASE_URL=[https://social.yourdomain.com](https://social.yourdomain.com)
      - HMAC_SECRET=replace_with_a_long_random_secret_string
      - POLL_INTERVAL_MINUTES=5
      # Weekly digest schedule: 0 = Sunday, 18 = 6:00 PM local time
      - DIGEST_WEEKDAY=0
      - DIGEST_HOUR=18
      # Prevent emailing historical workouts on first boot
      - SEED_EXISTING_WITHOUT_EMAIL=true
      # Mailgun credentials
      - MAILGUN_DOMAIN=mg.yourdomain.com
      - MAILGUN_API_KEY=key-your-mailgun-private-api-key
      - MAILGUN_FROM=YourName Workouts <workouts@mg.yourdomain.com>
      - MAILGUN_API_BASE=[https://api.mailgun.net](https://api.mailgun.net) # Use [https://api.eu.mailgun.net](https://api.eu.mailgun.net) for EU domains
      # ntfy push notification topic for instant Kudos alerts
      - NTFY_TOPIC_URL=[https://ntfy.sh/your_secret_kudos_topic](https://ntfy.sh/your_secret_kudos_topic)
      # Seed friends (slug:DisplayName:email:initial_pref)
      - FRIENDS=alice:Alice:alice@example.com:instant,bob:Bob:bob@example.com:weekly
    ports:
      - "127.0.0.1:8090:8080"
```

Start the container:

```bash
docker compose up -d dreeve-social
```

Check the startup logs to grab each friend's personal Preferences & RSS dashboard URL:

```bash
docker logs dreeve-social
```

---

## ⚙️ Configuration Reference

All configuration is passed via environment variables:

| Variable | Default | Description |
|---|---|---|
| `DREEVE_URL` | `http://dreeve:8080` | Internal or external base URL of your Dreeve instance. |
| `DREEVE_API_TOKEN` | *(required)* | Your Dreeve API Bearer token (`drv_...`). |
| `PUBLIC_BASE_URL` | `http://localhost:8090` | Public URL where `dreeve-social` is reachable by friends (used in email and RSS links). |
| `HMAC_SECRET` | *(required)* | Secret string used to sign and verify all `/kudos`, `/preferences`, and `/feed.xml` links. |
| `ATHLETE_NAME` | `Alex` | Your display name shown in emails, RSS feeds, and Kudos pages. |
| `FRIENDS` | *(empty)* | Comma-separated list of friends to seed into SQLite (see format below). |
| `POLL_INTERVAL_MINUTES` | `5` | How often (in minutes) to poll Dreeve for new activities. |
| `DIGEST_WEEKDAY` | `0` | Day of the week to send Weekly Digest emails (`0` = Sunday, `1` = Monday, ..., `6` = Saturday). |
| `DIGEST_HOUR` | `18` | Hour of the day (`0`–`23` in container's `TZ`) to send Weekly Digest emails. |
| `SEED_EXISTING_WITHOUT_EMAIL` | `true` | On a fresh database, records existing Dreeve activities without emailing friends about old workouts. |
| `MAILGUN_DOMAIN` | *(empty)* | Your Mailgun sending domain (e.g., `mg.yourdomain.com`). If omitted, emails log in dry-run mode. |
| `MAILGUN_API_KEY` | *(empty)* | Your Mailgun API key. |
| `MAILGUN_FROM` | `Dreeve Social <noreply@example.com>` | `From` header for outgoing emails. |
| `MAILGUN_API_BASE` | `https://api.mailgun.net` | Mailgun API base URL (set to `https://api.eu.mailgun.net` for EU regions). |
| `NTFY_TOPIC_URL` | *(empty)* | Full URL to your `ntfy` topic (e.g., `https://ntfy.sh/my_secret_topic`) for Kudos notifications. |
| `NTFY_TOKEN` | *(empty)* | Optional Bearer token if using a protected self-hosted `ntfy` instance. |
| `DB_PATH` | `/data/sidecar.db` | Path to the SQLite database file inside the container. |
| `LISTEN_ADDR` | `:8080` | HTTP listen address inside the container. |

---

## 👥 Seeding & Managing Friends (`FRIENDS`)

The `FRIENDS` environment variable accepts a comma-separated list of subscribers in any of these formats:

* **Full format:** `id:DisplayName:email:initial_pref`
  * Example: `alice:Alice Smith:alice@example.com:instant,bob:Bob:bob@example.com:weekly`
* **Shorthand format (defaults to `instant`):** `id:email`
  * Example: `alice:alice@example.com,bob:bob@example.com`

Valid preferences are:
* `instant` — Receives an email right after each new workout is uploaded.
* `weekly` — Receives a single weekly digest email summarizing the week's workouts.
* `none` — Unsubscribed from emails (can still use personal RSS feeds or re-subscribe anytime).

> **Note:** On container restart, `dreeve-social` will add new friends and update names/email addresses from `FRIENDS`, but it **never overwrites** a friend's `pref` column once they exist in SQLite—so changes your friends make on the `/preferences` page persist across restarts and upgrades.

---

## 🌐 HTTP Endpoints

Expose `dreeve-social` behind your reverse proxy (Caddy, Nginx, Traefik, or Cloudflare Tunnel). All user-facing routes require a valid HMAC signature (`sig`):

* `GET /healthz` — Returns `200 OK` (`ok`) for container health checks.
* `GET /kudos?act=<id>&who=<slug>&sig=<hmac>` — Renders the celebration page and triggers a browser `POST` to `/api/kudos`.
* `POST /api/kudos` — Verifies the HMAC signature, deduplicates the kudos in SQLite, returns the list of friends who cheered the workout, and sends an `ntfy` push alert.
* `GET /preferences?who=<slug>&sig=<hmac>` — Self-service portal to switch between Instant emails, Weekly Digest emails, or Unsubscribe/Re-subscribe, plus personal RSS feed links.
* `POST /preferences` — Updates the subscriber's email preference in SQLite.
* `GET /feed.xml?who=<slug>&mode=<instant|weekly>&sig=<hmac>` — Serves the subscriber's personalized RSS 2.0 feed with embedded 1-click Kudos links.

---

## 🛠️ Building Locally

### With Docker

```bash
docker build -t dreeve-social:local .
```

### Without Docker (Go 1.24+)

```bash
CGO_ENABLED=0 go build -ldflags="-s -w" -o dreeve-social main.go
./dreeve-social
```

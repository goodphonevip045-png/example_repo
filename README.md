# Restaurant Booking

## Run with Docker Compose

1. Install Docker Engine with the Compose plugin on the host.
2. Copy `.env.example` to `.env` and replace both passwords with different, long, random alphanumeric values. Keep `.env` private.
3. Start the app with `docker compose up -d --build`.
4. Open `http://HOST:8080` (or the port set by `HOST_PORT`).

The MySQL container initializes its schema from `db/init.sql` on the first start and stores data in the `mysql_data` volume. The app waits for MySQL's health check before starting. The application requires `DB_DSN`; Compose supplies it using the app database account.

## Deploy to Render

The repository includes `render.yaml` for the web service. Push the project to a Git repository that Render can access. The web service uses the Dockerfile, listens on Render's `PORT`, and checks `/healthz` before receiving traffic.

Render does not provide managed MySQL, so create a separate **Private Service** for MySQL first:

1. Create the service from this same repository with Docker runtime and `Dockerfile.mysql` as its Dockerfile. Use the Singapore region.
2. Set `MYSQL_DATABASE=restaurant_db`, `MYSQL_USER=restaurant_app`, and unique strong alphanumeric values for `MYSQL_PASSWORD` and `MYSQL_ROOT_PASSWORD`.
3. Attach a persistent disk at `/var/lib/mysql` (at least 10 GB). The MySQL image imports `db/init.sql` when the database volume is first initialized. Render persistent disks require a paid service plan.
4. Create a Render Blueprint from this repository. When prompted for `DB_DSN`, enter `restaurant_app:YOUR_DB_PASSWORD@tcp(YOUR_MYSQL_PRIVATE_HOST:3306)/restaurant_db`, replacing the host with the private hostname shown for the MySQL service. Keep the password alphanumeric so it does not need MySQL DSN escaping.

The blueprint sets `PORT=10000`, `TRUST_PROXY=true`, and the `/healthz` health check. Render terminates public HTTPS before forwarding requests to the web service. The `free` web plan can spin down while idle; use a paid web plan if you need it to stay responsive continuously. The MySQL service must retain its persistent disk across deploys.

To promote your first account on Render, open the MySQL private service's Shell and run `mysql -u restaurant_app -p restaurant_db`, then enter the MySQL app password and run the `UPDATE users SET role = 'admin' ...` statement shown below.

Register a user through the website. To make that account an administrator, connect to MySQL with `docker compose exec db mysql -u restaurant_app -p restaurant_db`, enter the `DB_PASSWORD` from `.env`, then run:

```sql
UPDATE users SET role = 'admin' WHERE email = 'your-email@example.com';
```

## Hosting behind HTTPS

Put a reverse proxy such as Caddy, Nginx, or a hosting provider's proxy in front of the app and enable HTTPS there. Set `TRUST_PROXY=true` only when the app is reachable exclusively through a trusted proxy that overwrites `X-Forwarded-Proto`. Restrict direct access to the app port if the host firewall or proxy can do so.

## Operations

- View logs with `docker compose logs -f app`.
- Stop the services with `docker compose down`. This preserves the database volume.
- Back up the MySQL database volume or export the database regularly.
- `db/init.sql` runs only when MySQL creates a fresh data directory. Existing databases need an explicit migration if the schema changes.

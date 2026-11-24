# notes

A small personal page with a WebSocket echo endpoint.

Endpoints:

- `/` — the page
- `/health` — `ok`
- `/version` — version string
- `/ip` — the address this process reaches the internet from
- `/ws` — WebSocket echo

Built as a single static Go binary; see `Dockerfile`.

# ⚡ NodePulse

> **Distributed infrastructure monitoring, Docker telemetry & incident control plane — open core, self-hostable.**

[![status](https://img.shields.io/badge/status-live-brightgreen)](#deployment)
[![go](https://img.shields.io/badge/go-1.24-blue)](#tech-stack)
[![docker](https://img.shields.io/badge/docker-monitored-2496ED)](#docker-telemetry)
[![license](https://img.shields.io/badge/license-MIT-green)](#license)

NodePulse is a self-hosted monitoring platform for Linux servers, Docker containers and the incidents that come out of running them in production. It pairs a tiny Go-based edge agent with a central control plane that gives you a single dashboard for every node you operate.

**Live instance:** [https://pulse.nqai.es-cloud.ru](https://pulse.nqai.es-cloud.ru)
**Mirror of this README in Russian:** [README.md](README.md)

---

## ⚡ Connect a node in one command

```bash
curl -sSL https://pulse.nqai.es-cloud.ru/install.sh | sh
```

That single line installs the agent, wires it to your Docker socket, registers it with the central plane and starts the systemd unit. The agent binary is < 10 MB resident, no external CLI dependencies.

If you already have an API token from the dashboard, pass it along:

```bash
curl -sSL "https://pulse.nqai.es-cloud.ru/install.sh?token=YOUR_API_TOKEN" | sh
```

The installer:

1. Downloads the correct `nodepulse-agent` binary for the host architecture.
2. Attaches to `/var/run/docker.sock` for direct Docker telemetry.
3. Installs and enables the `nodepulse-agent.service` systemd unit.

---

## ✨ What it gives you

- **Zero-dependency edge agent** — single static Go binary, < 10 MB RAM, < 0.1% CPU idle.
- **Direct Docker telemetry** — container discovery, health, uptime and OOM events through the Docker Unix socket; no extra CLI or sidecar required.
- **Multi-tenant by design** — account signup, scoped API tokens and strict per-user node isolation out of the box.
- **Incident & alert engine** — fires to Telegram and to a generic webhook on critical signals (RAM > 92%, OOM kills, container deaths); alerts resolve from the dashboard.
- **Prometheus exposition** — native `/metrics` endpoint on the control plane for plugging into existing monitoring.
- **Mobile-first UI** — single-file HTML, no SPA framework, with inline SVG sparklines for load history. Fast on phones, faster on desktops.

---

## 🚀 Self-host the control plane

```bash
git clone https://github.com/NQAI-Dev/nodepulse.git
cd nodepulse
docker compose up -d
```

The control plane listens on `:8080` by default. Agents then point at it via:

```bash
nodepulse-agent \
  -node my-host-01 \
  -server https://your-control-plane.example/api/v1/ingest \
  -token YOUR_API_TOKEN
```

See `deploy/` for systemd unit templates and a sample nginx front.

---

## 🧱 Tech stack

| Layer        | Choice                                            |
|--------------|---------------------------------------------------|
| Edge agent   | Go 1.24, single static binary, no cgo              |
| Server       | Go 1.24, `net/http` + SQLite (WAL mode)            |
| Frontend     | Vanilla HTML / JS, SVG sparklines, no framework   |
| Telemetry    | Docker Unix socket + `/proc` + `cgroup`           |
| Auth         | Per-user API tokens, scoped to node ownership      |
| Storage      | SQLite with `busy_timeout=5000` + WAL             |
| Observability| Native Prometheus `/metrics` + Telegram alerts    |

---

## 🗺 Architecture (one paragraph)

The edge agent runs as a systemd service on each monitored host, samples `/proc`, `cgroup` and `/var/run/docker.sock`, and pushes a compact batch over HTTPS to the control plane's ingest endpoint. The control plane stores time-series, evaluates alert rules on each ingest, fires Telegram/webhook notifications on threshold crossings, and exposes a per-account dashboard with the incidents and resolutions. Everything except Telegram is open and self-hostable.

```
┌─────────────┐   batched HTTPS    ┌─────────────────────────┐
│  edge agent │  ───────────────►  │   control plane (:8080)  │
│ (systemd)   │  every 5s          │  SQLite WAL + Prometheus │
└──────┬──────┘                    └────────────┬────────────┘
       │  reads                              │  serves
       ▼                                     ▼
  /proc, /cgroup,                       dashboard UI,
  /var/run/docker.sock                  /metrics, alerts
```

---

## 🧪 Local development

```bash
go run ./cmd/server        # control plane on :8080
go run ./cmd/agent         # edge agent pointed at the local server
go test ./...              # unit tests across pkg/
```

The full test suite lives in `pkg/probe/*_test.go` (TCP / TLS / DNS probe coverage) and `pkg/store/*_test.go` (schema migrations + WAL contention).

---

## 🛣 Roadmap (open items)

- Multi-tenant billing: per-team isolation, ЮKassa + Telegram Stars paywall.
- Pro onboarding wizard on first signup: install.sh, agent token, first probe.
- English-language incident response playbooks under `docs/`.

---

## 🤝 Contributing

Issues and small PRs welcome. Run `go test ./...` before opening a PR and follow the commit-message style already in `git log` (descriptive Russian, not Conventional Commits).

---

## 📜 License

MIT License © 2026 [NQAI](https://github.com/NQAI-Dev)

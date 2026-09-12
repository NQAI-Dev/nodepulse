# Security Policy

## Supported Versions
| Version | Supported          |
| ------- | ------------------ |
| `main`  | :white_check_mark: |
| older   | :x:                |

Only the latest commit on `main` receives security fixes. Production users should pin to a specific commit and bump manually.

## Reporting a Vulnerability
**Please do not file a public issue.**

Email `nqao-dev@mail.ru` (PGP key on request) with:
1. A description of the vulnerability and its impact.
2. Steps to reproduce, including version/commit hash.
3. Whether you intend to disclose publicly and on what timeline.

We aim to acknowledge within 72 hours and provide a fix or mitigation plan within 14 days, depending on severity and complexity.

## Threat Model
NodePulse components trust the local agent↔server channel as long as:
- The server uses HTTPS with a valid certificate.
- The agent's `NODE_ID` + `INGEST_TOKEN` are kept secret.

If an attacker controls the network between agent and server, they can:
- Read metric payloads (heartbeats are not end-to-end encrypted).
- Forge remediations only if the control-plane endpoint is exposed without a token.

We do not consider these in-scope for security fixes; mitigate them at the deployment layer (WireGuard / Tailscale / firewall rules).

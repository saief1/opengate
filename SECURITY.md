# Security Policy

OpenGate holds credentials for every model provider you use, so we treat security issues as top priority.

## Reporting a vulnerability

Please **do not open public issues for security problems.** Instead, report them privately through
GitHub's "Report a vulnerability" (Security Advisories) on this repository, or email the maintainers at the security contact listed on the project page (placeholder: set before publishing).
Include steps to reproduce, the affected version (`opengate version`) and the impact.

We aim to acknowledge reports within 2 business days and to ship a fix or mitigation for confirmed
high-severity issues within 14 days. We're happy to credit reporters.

## Supported versions

Until 1.0, only the latest release receives security fixes.

## Threat model (summary)

* **Protected:** provider API keys (AES-256-GCM at rest), virtual keys (stored hashed), prompt and response data
  (not stored by default), and spend (hard limits are enforced before requests reach a provider).
* **Trust boundary:** anyone holding `OPENGATE_ADMIN_TOKEN` or `OPENGATE_MASTER_KEY` together with the database is
  fully trusted. Keep both in a secret manager, never in the YAML config (it doesn't read them from there).
* **Out of scope:** a compromised host. Anyone who can read process memory can read decrypted provider keys.
  TLS termination is delegated to your reverse proxy.

## Hardening checklist

* Run behind TLS and keep the listener on localhost or a private network.
* Use `opengate keygen` for secrets and rotate the admin token periodically.
* Leave body logging off unless you need it, and restrict who can read the database file.
* Use the provided container image (distroless, non-root, read-only filesystem, no capabilities).
* Review `go.mod`: the project deliberately keeps two direct dependencies.

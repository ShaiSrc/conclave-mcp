# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: the repository's
**Security** tab → **Report a vulnerability**. Do not open a public
issue for a vulnerability.

Include what an attacker needs (a local API key, an allowed peer, a
visitor on the internet), the steps, and the version. You get an answer
within a week.

## Supported versions

Fixes go into the latest minor release. From 1.0 on, the latest minor
release of the current major version gets security fixes.

| Version | Security fixes |
|---|---|
| latest minor release | yes |
| older releases | no; upgrade with [the upgrade guide](docs/upgrading.md) |

## Security model

[docs/security.md](docs/security.md) describes the trust boundaries, what
each one relies on, and the limits that are known and accepted.

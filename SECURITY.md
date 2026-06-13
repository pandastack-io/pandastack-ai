# Security Policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately to `security@pandastack.ai`.

Include:

- affected component and version/commit
- reproduction steps or proof of concept
- impact assessment
- any logs or screenshots with secrets redacted

GPG key: TBD.

## Disclosure timeline

We target a 90-day coordinated disclosure window. We may ask for more time for complex fixes, but we will keep reporters updated.

## Rewards

PandaStack does not currently offer a paid bug bounty or rewards program. We still deeply appreciate responsible reports and will credit reporters when desired.

## In scope

- API authentication and authorization bypasses
- sandbox escape or cross-sandbox data access
- host agent privilege escalation
- secret leakage in logs, builds, or release artifacts
- supply-chain issues in supported build or release paths

## Out of scope

- denial-of-service without a demonstrated security boundary impact
- social engineering or physical attacks
- reports requiring access to someone else's account or infrastructure
- vulnerabilities in unsupported forks or modified deployments
- scanner-only findings without a reproducible impact

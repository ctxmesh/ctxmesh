# Security policy

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Use GitHub's **private vulnerability reporting** on this repository
(Security → Report a vulnerability). That routes the report privately to the maintainer and gives us a
place to coordinate a fix and a disclosure timeline with you.

If private reporting is unavailable to you for any reason, open a public issue containing **only** a
request to be contacted — no details, no reproducer — and we will take it from there.

## What to expect

This is currently a single-maintainer project working toward its first beta, so please calibrate:

- **Acknowledgement:** within a few days.
- **Assessment:** we will tell you whether we consider it a vulnerability, and why, rather than going
  quiet. A disagreement about severity is a conversation, not a rejection.
- **Fix and disclosure:** coordinated with you. We would rather be told about a real problem late than
  discover it publicly early.

## Scope

ctxmesh runs untrusted-ish agent workloads on Kubernetes and brokers credentials on their behalf, so the
areas where a bug is most likely to matter are:

- **Credential custody** — agent pods are designed to hold no long-lived secrets; a path that leaks one
  to an agent, a log, a trace, or a recorded fixture is in scope.
- **Tenant and namespace isolation** — anything that lets one tenant observe or affect another.
- **The capability model** — run capabilities are sender-constrained; a way to spend a leaked one is in
  scope.
- **Egress control** — a way for agent code to reach the network outside the sidecar.
- **Admission and policy** — a way to deploy an agent that bypasses a declared guardrail, budget, or
  approval.

**Out of scope:** the bundled *dev* data plane (in-cluster Valkey, MinIO, dex and the demo fixtures) is
explicitly a convenience tier for local development, is documented as not for production data, and ships
with well-known credentials on purpose. Findings there are expected rather than vulnerabilities.

## Supported versions

Pre-beta: only the tip of `main` is supported. There are no backports yet.

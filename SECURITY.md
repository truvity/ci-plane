# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately via
[GitHub Security Advisories](https://github.com/truvity/ci-plane/security/advisories/new).

Do NOT open a public issue for security vulnerabilities.

## Supported Versions

Only the latest release is supported with security updates.

## What is in scope

This repository publishes contracts, schemas, small configuration loaders and
a worked example. Reports that matter most:

- A loader that accepts a configuration it should refuse, or that reports a
  secret's value in an error or a log line.
- A contract or a schema whose defaults are unsafe for anyone who follows
  them.
- Anything in the example that would be a vulnerability in a real service,
  since the example is what people copy.

This repository holds no credentials and its CI runs on hosted runners with
no access to any private infrastructure. A finding that depends on a
particular deployment belongs with that deployment's owner.

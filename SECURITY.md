# Security Policy

## Supported Versions

We currently support the following versions with security updates:

| Version | Supported          |
| ------- | ------------------ |
| main    | :white_check_mark: |
| < main  | :x:                |

As Mercan is under active development, we recommend using the latest version from the `main` branch. Tagged releases will be added to this table once the project reaches a stable release cycle.

## Reporting a Vulnerability

We take the security of Mercan seriously. If you discover a security vulnerability, please help us by responsibly disclosing it.

### How to Report

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, please report them via email to:

**security@mercan.dev**

If you prefer encrypted communication, please request our PGP key in your initial contact email.

### What to Include

To help us understand and address the issue quickly, please include as much of the following information as possible:

- **Type of vulnerability** (e.g., authentication bypass, privilege escalation, injection, etc.)
- **Affected component(s)** (e.g., controller, worker pods, API endpoints, CRDs)
- **Step-by-step instructions** to reproduce the issue
- **Proof-of-concept or exploit code** (if applicable)
- **Impact assessment** — what could an attacker achieve by exploiting this vulnerability?
- **Suggested fix** (if you have one)
- **Your contact information** for follow-up questions

### Response Timeline

We are committed to responding promptly to security reports:

- **Initial Response**: Within 48 hours of your report
- **Status Update**: Within 7 days with our assessment and expected timeline
- **Resolution**: Varies by severity — critical issues will be prioritized for immediate fixes

We will keep you informed throughout the investigation and remediation process.

## Disclosure Policy

- We request that you give us reasonable time to investigate and address the vulnerability before public disclosure
- We will coordinate with you on the disclosure timeline and acknowledge your contribution (unless you prefer to remain anonymous)
- Once a fix is available, we will publish a security advisory on GitHub and credit the reporter (with permission)
- We follow a coordinated disclosure model and aim to release patches before or simultaneously with public disclosure

## Security Model

Mercan's security architecture includes:

- **Hardened containers** — All controller and worker pods run as non-root users with read-only root filesystems, dropped capabilities, and seccomp profiles
- **ServiceAccount token authentication** — All API endpoints require Kubernetes ServiceAccount bearer tokens validated via the TokenReview API
- **Secret management** — LLM API keys and credentials are stored in Kubernetes Secrets and never exposed in logs, task specs, or API responses
- **Namespace isolation** — Controller can be scoped to specific namespaces, with protections against operations in system namespaces
- **Audit logging** — All agent actions are logged as Kubernetes Jobs with full observability

For more details, see our [Security Documentation](docs/security.md).

## Security Best Practices for Users

When deploying and using Mercan:

1. **Limit RBAC permissions** — Grant only the minimum required permissions to ServiceAccounts used by Mercan components
2. **Rotate secrets regularly** — Update LLM API keys and credentials periodically
3. **Monitor agent activity** — Review task logs and results for unexpected behavior
4. **Use namespace isolation** — Deploy Mercan with `--watch-namespace` in production to limit its scope
5. **Keep up to date** — Regularly update to the latest version to receive security patches
6. **Enable network policies** — Restrict network access for worker pods where possible

## Contact

For non-security-related questions, please open a GitHub issue or discussion.

For urgent security concerns, email: **security@mercan.dev**

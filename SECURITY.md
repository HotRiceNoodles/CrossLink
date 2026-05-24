# Security Policy

## Reporting a Vulnerability

We take security seriously. If you discover a vulnerability in CrossLink, please report it responsibly.

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, please email your findings to the maintainers. Include:

- A description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

You can also use GitHub's [private vulnerability reporting](../../security/advisories/new) feature.

## Response Timeline

- We will acknowledge receipt within 48 hours.
- We will provide an initial assessment within 5 business days.
- We aim to release a fix within 30 days for confirmed vulnerabilities.

## Scope

This policy applies to the CrossLink Community Edition codebase in this repository.

## Security Best Practices for Deployment

When deploying CrossLink:

- Change all default credentials (`admin.password`, `admin.jwt_secret`, `gateway.auth_key`).
- Use strong, unique passwords and JWT secrets (32+ characters).
- Enable TLS for the admin dashboard and API endpoints.
- Restrict database and Redis access to trusted networks.
- Keep your dependencies up to date.
- Review and restrict `cors.allowed_origins` to known domains.

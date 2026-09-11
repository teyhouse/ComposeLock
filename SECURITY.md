# Security Policy

## Supported Versions

Only the latest release receives security updates.

| Version | Supported          |
| ------- | ------------------ |
| Latest  | :white_check_mark: |
| Older   | :x:                |

## Reporting a Vulnerability

Report security vulnerabilities privately via:

- **GitHub Security Advisories**: Use the "Report a vulnerability" tab on this repository
- **Email**: security@teyhouse.dev

Do not open public issues for security vulnerabilities.

### What to include

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

### Response timeline

- Acknowledgment within 72 hours
- Status update within 7 days
- Fix timeline depends on severity

## Security Measures

- Dependencies scanned with `govulncheck` on every CI run
- Minimal dependency surface (no unnecessary packages)
- Binary built with Go 1.27+ (modern crypto defaults)
- No secrets in config files (mode 0600 on init)
- Input validation at trust boundaries

## Disclosure

After a fix is released, a security advisory will be published on GitHub.
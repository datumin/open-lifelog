# Security Policy

## Supported versions

OLF is versioned per type with semver-style `major.minor`. The current major
line (**1.x**) is supported.

## Reporting a vulnerability

Please report security issues **privately** — do not open a public issue.

Use GitHub's private vulnerability reporting: the repository's **Security** tab →
**Report a vulnerability**. We aim to acknowledge reports within a few days.

Because OLF is a data format (not a running service), the relevant security
surface is mostly the JSON Schemas and validation guidance — for example a
schema that fails to constrain input as the spec intends, or guidance that could
lead an implementer to a validation bypass. Reports in that spirit are welcome.

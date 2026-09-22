# Security policy

Gopherdex stores other people's code and credentials, so we take reports seriously and fix them first.

## Reporting a vulnerability

**Please don't open a public issue or pull request.** Report privately through GitHub instead: go to the repository's **Security** tab, then **Report a vulnerability** ([direct link](https://github.com/parthiban-sivakumar/gopherdex/security/advisories/new)). Only the maintainers can see the report.

Include what you can:

- what an attacker can do, and what they need first (an account, a published module, nothing);
- steps to reproduce, or a proof of concept;
- the version or commit you tested.

## What happens next

- We acknowledge the report within **3 working days**.
- We confirm the issue, tell you how serious we think it is, and agree on a timeline, usually a fix within **30 days**, sooner for critical issues.
- We keep you updated, credit you in the advisory if you'd like, and publish a GitHub security advisory once a fixed release is out.

Please give us a reasonable time to fix the problem before you disclose it publicly.

## Scope

In scope: this repository's code, including the registry server (`gopherdexd`), the `gopherdex` CLI, and their default configuration.

Out of scope: denial of service by sheer traffic volume, findings that need an already-compromised server or administrator account, and reports from automated scanners without a demonstrated impact.

## Supported versions

Security fixes go into the latest release. Please test against it, or `main`, before reporting.

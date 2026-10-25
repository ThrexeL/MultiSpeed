# Security policy

## Supported versions

MultiSpeed is pre-alpha. There are no supported releases yet. Security fixes
will be applied to the `main` branch.

## Reporting a vulnerability

**Please do not open a public GitHub issue for security problems.**

Report vulnerabilities privately to the maintainers once the repository has a
published security contact. Until then, open a minimal public issue asking for
a private channel **without including exploit details, keys, or traffic
captures**.

Please include:

- a description of the issue and its impact,
- affected version or commit,
- reproduction steps or a proof of concept,
- whether the issue is already public,
- any suggested mitigation.

The maintainers will acknowledge receipt, investigate, and coordinate a fix and
disclosure timeline with you. Do not test against relays or networks you do not
own or have permission to test.

## Scope

In scope:

- the MultiSpeed protocol and Go implementation,
- cryptographic key handling and replay protection,
- the relay's authentication, forwarding, and isolation logic,
- privilege boundaries between the GUI, agent, and privileged helper,
- Windows route/DNS manipulation,
- Android VPN integration once available.

Out of scope:

- vulnerabilities in third-party dependencies that are already publicly known
  and not exploitable through MultiSpeed,
- denial of service requiring unbounded resources against a self-hosted relay,
- issues that require a compromised local administrator/root account,
- the security of an operator's relay host outside MultiSpeed's setup scripts.

## Safe harbor

We support good-faith security research on your own infrastructure. Do not
access other users' traffic, do not disrupt relays, and do not exfiltrate data.
We will not pursue legal action for research that follows this policy.

## Audit status

The code has not been independently audited. See
[docs/security.md](docs/security.md).

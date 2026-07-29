# Security policy

## Reporting a vulnerability

Please do not open a public issue for a vulnerability or include private source
material in a report.

Use a [private GitHub security
advisory](https://github.com/marcus/recall/security/advisories/new). If that
form is unavailable, email `marcus@vorwaller.net` with the subject
`Recall security report`. Include the affected version or commit, the
configuration needed to reproduce the problem, and a minimal proof that does
not contain your real corpus.

Recall is a personal open-source project. There is no response-time or fix-time
SLA. Reports will be handled as time permits, with priority given to issues
that can expose source material, cross a configured trust boundary, or execute
an untrusted adapter command.

## Security boundary

Recall reads a user's notes, task stores, and other private corpora. The most
sensitive source in a configured profile sets the practical sensitivity
ceiling for the process and for any logs, caches, evaluation artifacts, or
responses derived from it. Treat those artifacts accordingly.

Two controls are load-bearing:

- Profiles set a maximum sensitivity. A project configuration may narrow that
  ceiling but cannot raise it, and an adapter may raise a candidate's
  classification but cannot lower its source's floor.
- Executable adapter commands are accepted only from user-level configuration.
  A repository's `recall.toml` may tune bounded policy, but it cannot declare
  an adapter command or redirect a trusted source to another program.

Retrieved text is untrusted data. The HTTP and MCP surfaces label it that way;
agents must not follow instructions found inside retrieved excerpts or expanded
evidence.

Recall is designed to report denied sources and withheld records rather than
silently omit them. A bug that turns a denial, timeout, partial scan, or
sensitivity suppression into an ordinary empty result is security-relevant
because it changes what the caller believes was searched.

## Supported versions

Until Recall reaches 1.0, security fixes are made on the current `main` branch
and the newest tagged release when practical. Older pre-1.0 versions are not
maintained.

# Homebrew formula publication

Recall's tap formula builds from source so macOS users do not receive unsigned
prebuilt binaries that trigger Gatekeeper warnings. Formula publication uses
the local release operator's authorization because it changes another
repository; the tag workflow's token remains read-only.

The supported path is:

```sh
make release RELEASE_VERSION=vX.Y.Z
```

It waits for and verifies the exact tag workflow and public release, downloads
the exact public tag archive, renders and validates the formula in a temporary
tap clone, retries a racing push by rebasing without force, and verifies the
remote formula. If the tag exists but publication stopped, resume with:

```sh
make release-tap RELEASE_VERSION=vX.Y.Z
```

See [`docs/releasing.md`](../../docs/releasing.md) for credential prerequisites,
the complete guarded path, and manual recovery.

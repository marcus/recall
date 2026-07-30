# Releasing Recall

Recall releases are local, operator-authorized transactions across two
repositories: the tag workflow publishes GitHub release assets, then the
operator publishes the source-building formula to `marcus/homebrew-tap`.
`make release` owns both steps so a successful tag cannot be mistaken for a
complete release while Homebrew is still stale.

## One-time operator setup

Install `brew`, `curl`, `gh`, `git`, `jq`, `ruby`, `tar`, `shasum` (or
`sha256sum`), and Go/GoReleaser as required by the normal release preflight.
Configure a Git author name and email.

Authenticate GitHub CLI and Git using an operator credential that can:

- read Actions, releases, and repository contents in `marcus/recall`;
- read and write repository contents in `marcus/homebrew-tap`.

A fine-grained personal access token limited to those two repositories and
permissions, or an SSH key with equivalent repository access, is preferred.
Keep the credential in the operator's GitHub CLI/credential-store setup. Do
not copy a broad local `gh` token into an Actions secret: the tag workflow's
read-only default token intentionally cannot write another repository.

Confirm `gh auth status --hostname github.com` and that `gh`'s configured Git
protocol can clone and push `marcus/homebrew-tap`. The automation also checks
these prerequisites immediately before it creates a tag.

## Cut a release

Prepare and push the release commit on `main`, including the version's
`CHANGELOG.md` entry, and satisfy the repository's required CI/fork checks.
From a clean checkout at the live `origin/main` commit:

```sh
make release RELEASE_VERSION=vX.Y.Z
```

The target:

1. runs the full local release preflight;
2. verifies operator tools, GitHub access, and tap push permission;
3. creates and pushes the annotated tag;
4. waits for the exact `release.yml` run whose tag and commit match;
5. requires that workflow and the corresponding GitHub release to succeed;
6. downloads the exact public tag source archive and calculates its SHA-256;
7. renders the formula using the renderer and template inside that tagged
   archive, so later `main` changes cannot alter a recovery publication;
8. runs Ruby syntax, `brew style`, and strict online `brew audit` checks;
9. commits and pushes only `Formula/recall.rb` in a temporary tap clone;
10. rebases and retries a racing tap push without force-pushing; and
11. fetches `origin/main` again and verifies its formula version, checksum, and
    complete rendered content.

The command is intentionally blocking. It is not complete until the final
remote formula verification succeeds.

## Recovery after the tag exists

If the workflow, network, Homebrew validation, or tap push fails after the tag
was pushed, fix the reported cause and resume only the idempotent post-tag
portion:

```sh
make release-tap RELEASE_VERSION=vX.Y.Z
```

This re-verifies the public annotated tag, the exact successful workflow, the
public release, and the archive checksum. It refuses to downgrade a newer tap
formula or overwrite a same-version formula that differs from the exact
rendered result. A clean race on unrelated tap changes is rebased and retried;
a formula conflict stops for manual investigation.

If publication remains blocked, render a candidate for inspection without
hand-editing it:

```sh
curl -fL -o /tmp/recall-vX.Y.Z.tar.gz \
  https://github.com/marcus/recall/archive/refs/tags/vX.Y.Z.tar.gz
shasum -a 256 /tmp/recall-vX.Y.Z.tar.gz
tar -xzf /tmp/recall-vX.Y.Z.tar.gz -C /tmp
/tmp/recall-X.Y.Z/scripts/render-homebrew-formula.sh \
  vX.Y.Z <reported-sha256> /path/to/homebrew-tap/Formula/recall.rb
```

Homebrew only audits formulae inside a tap. Put the candidate in a disposable
local tap, validate it, and remove the tap afterward:

```sh
brew tap-new --no-git recall-release/manual-validation
mkdir -p \
  "$(brew --repository recall-release/manual-validation)/Formula"
cp /path/to/homebrew-tap/Formula/recall.rb \
  "$(brew --repository recall-release/manual-validation)/Formula/recall.rb"
brew style --formula recall-release/manual-validation/recall
brew audit --strict --online \
  --formula recall-release/manual-validation/recall
brew untap --force recall-release/manual-validation
```

Then use a normal reviewed tap commit. Never force-push the tap. Finally rerun
`make release-tap RELEASE_VERSION=vX.Y.Z`; its idempotent remote proof should
pass before the release is considered complete.

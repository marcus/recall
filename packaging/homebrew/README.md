# Homebrew formula publication

Recall's tap formula builds from source so macOS users do not receive unsigned
prebuilt binaries that trigger Gatekeeper warnings. Formula publication is
deliberately separate from the tag workflow: it changes another repository and
requires its own authorization.

After the tag and GitHub release exist:

1. Download
   `https://github.com/marcus/recall/archive/refs/tags/vX.Y.Z.tar.gz` and
   calculate its SHA-256.
2. Use a clean checkout of `marcus/homebrew-tap`.
3. Render the formula atomically:

   ```sh
   ./scripts/render-homebrew-formula.sh \
     vX.Y.Z <64-character-sha256> \
     /path/to/homebrew-tap/Formula/recall.rb
   ```

4. Review the rendered diff, run `brew style` and `brew audit`, then commit and
   push the tap change only with explicit authorization.

The renderer validates strict SemVer, the checksum shape, the output filename,
all template substitutions, and Ruby syntax before replacing `recall.rb`.

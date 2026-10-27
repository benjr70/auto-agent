# Verify a release binary with a GitHub artifact attestation, swap it in by rename

Ticket: [#200](https://github.com/benjr70/auto-agent/issues/200) (part of wayfinder map
[#199](https://github.com/benjr70/auto-agent/issues/199) — Map: installing and upgrading the release binary safely).
Researched on 2026-10-27.

Sources: `.github/workflows/release.yml`, `README.md` (Install, The service),
https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations,
https://docs.github.com/en/actions/concepts/security/artifact-attestations,
https://cli.github.com/manual/gh_attestation_verify,
https://docs.sigstore.dev/cosign/signing/signing_with_blobs/,
https://docs.sigstore.dev/cosign/verifying/verify/,
https://man7.org/linux/man-pages/man2/rename.2.html,
https://man7.org/linux/man-pages/man2/open.2.html,
https://raw.githubusercontent.com/coreutils/coreutils/master/src/install.c

## TL;DR

- **Recommend GitHub artifact attestations**, and keep `SHA256SUMS` as the
  no-tooling fallback. `SHA256SUMS` is published by the same job, with the same token, into the
  same Release as the binary, so it catches a corrupt download but not a
  tampered Release. An attestation is a Sigstore-signed provenance statement
  tied to the repo and the workflow that built the binary. For a solo
  maintainer it costs one step and two permissions in `release.yml`, with no keys to look after,
  and the operator checks it with the `gh` they already have. Cosign keyless
  uses the same Sigstore machinery but makes us publish the bundles and makes the operator install
  a second tool. Cosign with a key makes the maintainer guard a long-lived private key.
- **Swap by rename, never by writing over the file**: download and verify in
  `~/.local/bin` under a temporary name, `chmod 0755`, `mv -f` it onto
  `~/.local/bin/auto-agent` (one `rename(2)`, atomic on the same filesystem),
  then `systemctl --user restart auto-agent-<owner>.<name>.service`.
- Follow-up needed: `release.yml` does not attest anything today. Add the
  step below before the README can tell operators to run `gh attestation verify`.

## What ships today

`release.yml` builds four binaries, writes `SHA256SUMS` with
`sha256sum auto-agent-* > SHA256SUMS`, and runs
`gh release create … dist/*` with `permissions: contents: write`
(`.github/workflows/release.yml`). The README's Install section downloads the
binary and `SHA256SUMS` from the same Release URL, runs
`sha256sum --ignore-missing -c SHA256SUMS`, then
`install -m 0755 … ~/.local/bin/auto-agent`. To upgrade, it says to install the
new release over the old one and restart the unit (`README.md`, Install). The
unit runs the binary by its absolute path (`README.md`, The service).

## The three options compared

| | `SHA256SUMS` (today) | GitHub artifact attestation | cosign / Sigstore |
| --- | --- | --- | --- |
| Proves | the bytes match a list in the same Release | the bytes were built by a named workflow in `benjr70/auto-agent`, signed through Sigstore | keyless: same as attestation; keyed: whoever holds the key signed it |
| Workflow change | none | `actions/attest` step + `id-token: write`, `attestations: write` | install cosign, `cosign sign-blob --bundle`, upload the bundles |
| Maintainer secrets | none | none (OIDC) | keyless: none; keyed: a private key and its password |
| Operator tooling | `sha256sum` | `gh` | `cosign` |

- **Checksums.** The workflow writes the sums file and the binaries together
  and publishes them in one `gh release create` (`release.yml`). So anything that can
  replace a binary in the Release can also replace its line in `SHA256SUMS`.
  The file checks integrity, not origin.
- **Attestations.** The workflow needs `id-token: write`,
  `contents: read` and `attestations: write`, plus a step that uses the `attest` action with
  `subject-path` set to the binary. The operator verifies with
  `gh attestation verify PATH -R OWNER/REPO`
  ([how-to](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations)).
  Public repositories use the Sigstore Public Good Instance and its public
  transparency log. An attestation links an artifact to its source and build, and is
  "*not* a guarantee that an artifact is secure"
  ([concepts](https://docs.github.com/en/actions/concepts/security/artifact-attestations)).
  `verify` needs `--repo` or `--owner`. The manual calls checking the signer
  workflow (`--signer-workflow`) ideal, and the OIDC issuer defaults to
  `https://token.actions.githubusercontent.com`. `--bundle` allows offline
  verification ([gh manual](https://cli.github.com/manual/gh_attestation_verify)).
- **cosign.** Keyless `cosign sign-blob <file> --bundle bundle.sigstore.json`
  gives a bundle that "must" be stored for later verification. Key-based
  signing needs a private key and its password
  ([signing blobs](https://docs.sigstore.dev/cosign/signing/signing_with_blobs/)). The operator runs
  `cosign verify-blob <file> --bundle … --certificate-identity=… --certificate-oidc-issuer=…`
  ([verify](https://docs.sigstore.dev/cosign/verifying/verify/)). That is the
  attestation guarantee, with more steps on both sides.

## Recommendation

Attest every release binary with GitHub artifact attestations, and have operators
verify with `gh attestation verify` pinned to this repo and to the release
workflow. Keep `SHA256SUMS` for a quick integrity check where `gh` is
missing. It is the only option that ties the binary to the
`benjr70/auto-agent` release workflow without a key for a solo maintainer to
guard, and without a second tool for the operator. The Sigstore guarantee
cosign would add is the same one, so cosign only adds work here. Then install by writing
a temporary file in `~/.local/bin` and renaming it over the old binary, so the
running unit never sees a half-written or missing file. Restart the unit to
pick it up.

Change to `release.yml` (the follow-up):

```yaml
permissions:
  contents: write
  id-token: write
  attestations: write
# … after Build, before Publish:
      - uses: actions/attest@v4
        with:
          subject-path: 'dist/auto-agent-*'
```

Install or upgrade on a Host:

```sh
tag=v1.2.3 platform=linux-amd64 unit=auto-agent-<owner>.<name>.service
base=https://github.com/benjr70/auto-agent/releases/download/$tag
mkdir -p ~/.local/bin && cd ~/.local/bin
tmp=.auto-agent.$tag.new                       # same directory = same filesystem
curl -fL -o "$tmp" "$base/auto-agent-$tag-$platform"
gh attestation verify "$tmp" -R benjr70/auto-agent \
  --signer-workflow benjr70/auto-agent/.github/workflows/release.yml
chmod 0755 "$tmp"
"./$tmp" version                               # auto-agent v1.2.3 (commit …)
mv -f "$tmp" auto-agent                        # one rename(2): atomic swap
systemctl --user restart "$unit"
```

If any step fails, `rm -f "$tmp"` and the old binary stays in place, untouched.

## Why rename, not `install` or `cp`

- `rename(2)`: when `newpath` exists it "will be atomically replaced", and "there
  is no point at which another process attempting to access newpath will find
  it missing". It fails with `EXDEV` across filesystems, which is why the
  temporary file goes in `~/.local/bin` and not `/tmp`
  ([rename(2)](https://man7.org/linux/man-pages/man2/rename.2.html)).
- coreutils `install` sets `unlink_dest_before_opening = true` and copies
  straight to the destination, with no temporary file and no rename
  ([install.c](https://raw.githubusercontent.com/coreutils/coreutils/master/src/install.c)).
  So the README's `install … ~/.local/bin/auto-agent` leaves a window with no
  binary, then a partly written one. A unit restart (`Restart=on-failure`,
  `README.md`) in that window would exec a broken file.
- Writing in place over a running executable fails with `ETXTBSY`, because the "executable
  image … is currently being executed and write access was requested"
  ([open(2)](https://man7.org/linux/man-pages/man2/open.2.html)). Rename never
  opens the old file. The running Daemon keeps its old inode until the restart.

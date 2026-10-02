# Security

## Reporting a vulnerability

Please report security issues privately via GitHub's **Report a vulnerability**
feature (Security tab → Report a vulnerability). Do not open public issues for
undisclosed vulnerabilities. We aim to acknowledge reports within 72 hours.

## Supply-chain controls

This project ships with a defense-in-depth, 100% open-source pipeline. Every
control below runs on GitHub Actions and is free for public repositories.

| Stage | Control | Where |
|-------|---------|-------|
| Actions integrity | All actions pinned to immutable commit SHAs; kept fresh by Dependabot | every workflow, `.github/dependabot.yml` |
| Build-time egress | StepSecurity Harden-Runner (audit) on every job | all workflows |
| Go dependency CVEs | `govulncheck` + Trivy filesystem scan | `ci.yml`, `release.yml` (gate) |
| PR dependency hygiene | Dependency Review (severity + license policy) | `supply-chain-pr.yml` |
| Secrets | Gitleaks | `supply-chain-pr.yml` |
| SAST | CodeQL (`security-extended`) | `codeql.yml` |
| Repo posture | OpenSSF Scorecard | `scorecard.yml` |
| Release gate | `guard` job must pass before any artifact is built/pushed | `release.yml` |
| Tamper-evidence | Cosign keyless signatures + SLSA build provenance + SBOM | `release.yml`, `.goreleaser.yaml` |

### The release gate

A compromised or vulnerable dependency cannot produce a published artifact:
the `guard` job (`go mod verify` + `govulncheck` + Trivy) must succeed before
the `release` job that builds, pushes, and signs anything runs (`needs: guard`).

## Verifying a release

**SLSA build provenance** (container image):

```sh
gh attestation verify \
  oci://ghcr.io/nandotorres/argocd-oci-generator-plugin:<version> \
  --repo nandotorres/argocd-oci-generator-plugin
```

**Image signature** (cosign keyless / Sigstore):

```sh
cosign verify ghcr.io/nandotorres/argocd-oci-generator-plugin:<version> \
  --certificate-identity-regexp 'https://github.com/nandotorres/argocd-oci-generator-plugin' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

**Release archives** (checksum file is Sigstore-signed; cosign v4 bundle):

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/nandotorres/argocd-oci-generator-plugin' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
# then verify your archive against the now-trusted checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

## Running the gate locally

```sh
make audit   # go mod verify + govulncheck + trivy fs (same thresholds as CI)
```

## Keeping the toolchain current

The gate scans the Go standard library too, so an outdated toolchain will fail
it. Bump to the latest patch and re-run the gate in one step:

```sh
make go-upgrade    # pins the latest Go toolchain + stdlib, tidies, re-audits
make deps-upgrade  # updates module dependencies, tidies, re-audits
```

## Signed commits

Every commit on `main` must be cryptographically signed and verified. This is
enforced in two independent places:

- **Ruleset:** `required_signatures` rejects unsigned pushes at the server.
- **CI:** the `verify signed commits` job fails the build if any commit's
  GitHub verification status is not `verified` (covers SSH and GPG).

For GitHub to mark your signatures as *verified*, add your **public** signing
key to your account under *Settings → SSH and GPG keys → New signing key*
(this is separate from an authentication key, even if it's the same key).

Local setup used by this repo (SSH signing):

```sh
git config gpg.format ssh
git config user.signingkey ~/.ssh/<your-key>.pub
git config commit.gpgsign true
git config tag.gpgsign true
```

## Branch protection as code

Default-branch protection is declarative in `.github/rulesets/main.json`
(a GitHub repository ruleset) and applied idempotently with:

```sh
make ruleset       # requires `gh` authenticated with repo admin
```

It enforces: pull-request reviews (1 approval, stale-dismissal, last-push
approval, thread resolution), linear history, no force-push/deletion, and
**all CI/security checks green** before merge, so the supply-chain gate
cannot be bypassed on `main`.

# Releasing dlm

A release is made by pushing a version tag. GitHub Actions
(`.github/workflows/release.yml`) runs [GoReleaser](https://goreleaser.com) with
`.goreleaser.yaml`.

```sh
git tag -a v1.2.3 -m "v1.2.3"
git push origin v1.2.3
```

Tags with a suffix (`v1.2.3-rc1`) are published as GitHub pre-releases and are
not pushed to the Homebrew tap.

## What a release contains

| Artifact | Name |
|---|---|
| Archives (`tar.gz`, `zip` on Windows) for linux, darwin and windows on amd64 and arm64 — the binary, `LICENSE`, `NOTICE`, `README.md` and shell completions | `dlm_<version>_<os>_<arch>` |
| Debian and RPM packages for linux amd64 and arm64 (binary, completions, a systemd *user* unit) | `dlm_<version>_linux_<arch>.deb` / `.rpm` |
| SHA-256 checksums of all of the above | `dlm_<version>_checksums.txt` |
| The REST API's OpenAPI 3.1 document of that version, for client generators | `dlm_<version>_openapi.json` |
| Changelog from the commit messages (`feat`, `fix`, other) | the release notes |

Every binary is built with `CGO_ENABLED=0`, so the archives run on a bare system.

## Package managers

| Manager | Install | Needs |
|---|---|---|
| Homebrew (macOS, Linux) | `brew install sabadia/tap/dlm` | repository `sabadia/homebrew-tap` with a deploy key (write access to that repository only); its private key is the secret `HOMEBREW_TAP_DEPLOY_KEY` |
| Scoop (Windows) | `scoop bucket add sabadia https://github.com/sabadia/scoop-bucket` then `scoop install dlm` | repository `sabadia/scoop-bucket` with a deploy key; secret `SCOOP_BUCKET_DEPLOY_KEY` |
| winget (Windows) | `winget install sabadia.dlm` | the fork `sabadia/winget-pkgs` of `microsoft/winget-pkgs`; secret `WINGET_GITHUB_TOKEN`, a classic personal access token with the `public_repo` scope (it must open the pull request on `microsoft/winget-pkgs`, which no repository-scoped credential can); the first submission is reviewed by hand by the winget team |
| apt / dnf | install the downloaded `.deb` / `.rpm` | nothing: the packages are release assets, not a repository |

A publisher whose secret is not set is skipped, so a release works before the
repositories exist. The GitHub release itself only needs the built-in
`GITHUB_TOKEN`.

### Rotating a deploy key

```sh
ssh-keygen -t ed25519 -N "" -f key -C "dlm-server release"
gh repo deploy-key add key.pub --repo sabadia/homebrew-tap --title "dlm-server release workflow" --allow-write
gh secret set HOMEBREW_TAP_DEPLOY_KEY --repo sabadia/dlm-server < key
rm key key.pub       # then delete the old deploy key in the tap's settings
```

## Trying it without publishing

```sh
make snapshot     # goreleaser release --snapshot --clean --skip=publish
ls dist/
```

CI runs `goreleaser check` and a snapshot build on every pull request, so a
broken release configuration is found before a tag is pushed.

The OpenAPI document of any checkout is printed by `dlm openapi` (`make openapi`).

## Running the packaged daemon

The Debian and RPM packages install a systemd user unit:

```sh
systemctl --user enable --now dlm
```

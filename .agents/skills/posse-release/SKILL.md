---
name: posse-release
description: Publish a new Posse version from this repository when the User asks for a release or a weekly update. Choose the SemVer bump, publish the tag, and verify the install path.
---

# Posse release

Use this skill only for `Thanhbinh1905/posse`. The User's release request authorizes the necessary commit, push, tag, and GitHub publication. A weekly request starts one release attempt; it does not create a schedule.

1. Confirm the checkout, `origin`, branch, dirty state, latest published release, tags, and current `origin/main`. Use `gh-axi` for GitHub state. Preserve unrelated local changes and other worktrees. Release from the latest `origin/main`; bring release-related edits into `main` through the repository's normal review path if needed.
2. Compare commits since the latest published tag with the actual code and public behavior. Choose the next SemVer tag: major for an incompatible release, minor for compatible features, patch for fixes only. The User's explicit version choice takes precedence. Explain the selected bump briefly. If there is no releasable change, report that and stop without creating a duplicate release. Derive the version from published releases and Git tags, not `.release-please-manifest.json`.
3. Check CI for the exact commit being released and resolve failures before tagging. Inspect `.github/workflows/release.yml`, `.goreleaser.yaml`, and `install.sh` when any of them changed. Do not modify generated files or `CHANGELOG.md` by hand.
4. Create an annotated `vMAJOR.MINOR.PATCH` tag on the verified `main` commit and push that tag to `origin`. Only this explicit tag push starts the Release workflow; ordinary merges to `main` do not publish a release. The workflow runs GoReleaser and publishes the GitHub Release. Do not create a competing release while that workflow runs. Treat published tags as immutable; investigate and rerun a failed workflow when safe, or use a new version if the release commit must change.
5. Wait for the Release workflow to finish. Confirm the release is public and has `checksums.txt` plus Linux and macOS archives for amd64 and arm64. Install through the public `install.sh` URL with `sh -s -- --no-setup --prefix <temporary-directory>`; confirm SHA-256 verification and that the installed binary reports the chosen version. Keep the user's live Posse installation and setup untouched.
6. Report the tag, commit, release URL, workflow result, install command, and any unresolved limitation. State success only after the public download and installer work. If blocked, report the exact blocker and the next action instead of claiming a release.

Do not put credentials in command arguments or logs.

# Releasing spod

Releases are tags on `main`. There is no release branch: when the work
for a version is merged, tag it and push the tag. Branch conventions and
the local push guard live in CONTRIBUTING.md; this file is the
maintainer runbook.

## One-time repository setup

1. Branch protection via a ruleset (Settings → Rules → Rulesets → New
   ruleset → New branch ruleset):
   - Enforcement status: **Active** — it defaults to Inactive, which
     enforces nothing silently.
   - Target branches: `main`; bypass list empty (maintainers merge via
     pull requests like everyone else).
   - Require a pull request before merging (required approvals 0 is
     fine — the checks are the gate, not review).
   - Require status checks to pass:
     - `Test suite (in-container)`
     - `Binary (amd64)`
2. The check names must match the job `name:` fields in
   `.github/workflows/build.yml` exactly, and the picker only lists
   checks that ran within the last 7 days — open the first pull request
   before configuring the ruleset (its run populates the list).

## Cutting a release

1. Confirm `main` is green and holds what the version should ship:
   `git switch main && git pull`.
2. Choose the number: features bump the minor, fixes the patch.
3. Tag the merge commit on `main` and push the tag — the only release
   act; `main` itself is never pushed:

   ```sh
   git tag -a v0.2.0 -m "spod v0.2.0: one-line summary" && git push origin v0.2.0
   ```

4. The tag push is the whole release act: the Release workflow
   (`.github/workflows/release.yml`) rebuilds every supported GOARCH
   from the tagged commit — its build loop is the authoritative target
   list — verifies the binaries, and creates the release with them
   attached. Watch the run; the release page is the distribution point
   (one file per architecture). If a release must be redone, fix and move the tag
   (`git tag -f v0.2.0 main && git push -f origin v0.2.0`) — no release
   exists yet to clobber — or attach files to it by hand.
5. Verify the release binary reports exactly the tag: `spod --version`.

## How the version is decided at build time

The container build stamps `git describe --tags --dirty`: a checkout on
the tag produces the tag itself, commits after it produce
`vX.Y.Z-N-g<sha>`, and sources without usable git metadata fall back to
the Go toolchain's VCS stamp (`dev-<commit>`) or plain `dev`. Module
installs of a tagged release (`go install ...@v0.2.0`) report the module
version. The chain lives in `pkg/cli.buildVersion`.

## Maintaining CI locally

The workflows run under [act](https://github.com/nektos/act) before they
run on the forge; the canonical invocations are documented at the top of
`.github/workflows/build.yml`:

```sh
export DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
act pull_request -P ubuntu-latest=-self-hosted            # the PR path
act push -e /tmp/tag-event.json -P ubuntu-latest=-self-hosted   # a v* tag
```

Run act from a clean clone, never a worktree with untracked state — act
copies its working directory into the build as-is.

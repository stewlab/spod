# Contributing to spod

## Reporting issues

Open a GitHub issue with the spod version (`spod --version`) and the
exact commands plus full output. Security-sensitive reports go through
GitHub's private vulnerability reporting instead of a public issue.

## Development

- **Branch from `main`.** Keep branches short-lived and named by intent:
  `feat/<slug>`, `fix/<slug>`, `docs/<slug>`, `ci/<slug>`, `build/<slug>`,
  `chore/<slug>` (kebab-case slug). One concern per branch.
- **`main` accepts nothing but pull requests.** Never push directly to
  `main`, not even maintainers; branch protection rejects it server-side,
  and a local hook can refuse it before that (below). Releases are tags
  cut on `main`, pushed deliberately.
- Build and test in the container — no host toolchain needed:

  ```sh
  podman build --target test .                                  # full suite
  podman build --target binary --output type=local,dest=bin .   # binary
  bin/spod --version                                            # smoke it
  ```

- Commit subjects: lowercase imperative with a prefix (`feat:`, `fix:`,
  `docs:`, `ci:`, `build:`, `chore:`) — `git log` shows the house style.
  Keep commits granular; pull requests rebase-merge so `main` stays
  linear.

## Local push guard (recommended)

The repo ships `.githooks/pre-push`, which refuses any push to `main`
and points you at the tag command instead. Enable it locally:

```sh
git config core.hooksPath .githooks
```

## Pull requests

CI runs the same in-container suite and artifact build as checks on
every pull request targeting `main`; a red check blocks the merge. Keep
the PR title in the same style as commit subjects.

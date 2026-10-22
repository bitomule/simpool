# AGENTS.md

## After a release

A task that changes simpool is not finished when the PR merges. Pushing a `v*` tag
runs [`.github/workflows/release.yml`](.github/workflows/release.yml), which tests,
builds, publishes the GitHub release and rewrites the formula in `bitomule/homebrew-tap`.
Nothing upgrades the Mac afterwards, so finish the rollout yourself:

1. Wait until the release workflow has published the new version and the
   `bitomule/tap` formula is updated to it (`gh run list --workflow Release`, then
   `brew update && brew info bitomule/tap/simpool` shows the new version).
2. Run `brew update && brew upgrade simpool && brew services restart bitomule/tap/simpool`.
3. Check `simpool version --short` prints the new version and
   `brew services info simpool` shows `Running: true`.
4. Say whether Alondra's `git_override` of simpool in `~/Projects/alondra/MODULE.bazel`
   should move to the released commit too, and name the commit (`git rev-parse <tag>`).
   It only matters when the release changed `bazel/`.

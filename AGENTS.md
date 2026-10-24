# cproxy

## Build and test

CI runs these; run them before pushing:

```sh
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

## Commits and pull requests

Releases are cut by release-please from conventional commits on `main`
(`feat:` makes a minor release, `fix:` a patch, `feat!:` a major).

- Write every commit as a conventional commit: `<type>[(scope)][!]: summary`,
  with type one of feat, fix, perf, deps, revert, docs, style, chore,
  refactor, test, build or ci.
- **Give the PR title the same form.** PRs are squash-merged, and a squash
  merge of several commits takes the PR title as its commit subject, which is
  the only line release-please reads. The `PR title` workflow fails titles
  that are not conventional.
- A PR that both fixes and adds something is titled by its most significant
  change (`feat:` over `fix:`). To list each change in the changelog, add a
  `BEGIN_COMMIT_OVERRIDE` … `END_COMMIT_OVERRIDE` block to the PR description
  with one conventional message per line.

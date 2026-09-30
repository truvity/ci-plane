# Contributing

## Ground rules for a public repository

This repository is public and its history cannot be unpublished. Nothing in
it may name a real organisation, cluster, account, environment, team, person,
incident or internal ticket — in code, documents, tests, commit messages or
pull request text. Say "a consuming estate", "an environment", "the source
estate".

`hack/leak-canary.sh` catches the mechanical half of that and runs as part of `just check` and again in CI. It
cannot read prose, so the rest is a review rule. Quote a placeholder, never a
real value, including in a commit message: a message is as public as a file and
cannot be edited after the push.

## The gate

`just check` is the gate and needs no credentials and no cluster. It runs:

- `lint` lints both charts and checks the shell scripts' syntax
- `golden` renders every case, compares it with `tests/golden/` and proves every refusal fixture fails
- `leak-canary`

The Go tests of the helper modules under `image/` and the image builds are separate CI jobs.

A change to a chart regenerates the golden renders with `just golden`; review that diff in the pull request.

## Component contract

This repository is held to the
[component contract](https://github.com/truvity/policy/blob/master/docs/contracts/component.md);
its `policy-conformance` check runs in CI. A change that breaks a rule of the
contract fails there.

## Changelog

A pull request that changes what a consumer sees adds its bullet to
`CHANGELOG.md` under the version it will be tagged as, creating the heading if
it is the first. Every tag cut by hand has exactly one heading. A breaking
bullet starts with **Breaking:** and names the adoption step.

## Tooling

Tools come from `devbox.json` through direnv. Never hand-roll a PATH; add a
missing tool with `devbox add <pkg>@<version>`.

## Commits and pull requests

Small, reviewable pull requests. Pull requests merge by rebase, so a branch
carries no merge commits.

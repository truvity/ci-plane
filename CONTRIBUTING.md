# Contributing

## Ground rules for a public repository

This repository is public and its history cannot be unpublished. Nothing in
it may name a real organisation, cluster, account, environment, team, person,
incident or internal ticket — in code, documents, tests, commit messages or
pull request text. Say "a consuming estate", "an environment", "the source
estate".

`hack/leak-canary.sh` catches the mechanical half of that and runs on every
commit through lefthook, and again in CI. It cannot read prose, so the rest is
a review rule. Quote a placeholder, never a real value, including in a commit
message: a message is as public as a file and cannot be edited after the push.

## What a contract is

A contract in `docs/contracts/` is normative and testable. It says "must",
it is short enough to read in a sitting, and it carries a conformance section
naming how it is checked mechanically — a lint rule, a golden, a schema. A
rule with no way to check it is a preference, and belongs in a guide.

A canon in `docs/canon/` is an allowed list. It states its scope as a rule,
never as an enumeration of repositories, because an enumeration goes stale
and a stale canon is one people stop trusting.

A guide in `docs/guides/` is not normative and may change without a version
bump. It walks the example.

## Decisions

Every decision a stranger adopting these contracts would also face is
recorded under `docs/decisions/`. A decision that applies to only one
deployment is not recorded here. Decisions are never edited after acceptance;
they are superseded by a new one that links back.

## Changing a contract

A pull request that changes what a consumer must do adds its bullet to the
CHANGELOG under the version it will be tagged as, creating the heading if it
is the first. A breaking bullet starts with **Breaking:** and names the
adoption step.

The example in `examples/` is how a contract is proven. A contract that the
example does not satisfy is not finished, and a change to a contract changes
the example in the same pull request.

## Tooling

Tools come from `devbox.json` through direnv. Never hand-roll a PATH; add a
missing tool with `devbox add <pkg>@<version>`.

`just check` is the gate and needs no credentials, no container and no
cluster. Recipes that need more are separate, and CI runs them as their own
jobs.

## Commits and pull requests

Small, reviewable pull requests. Pull requests merge by rebase, so a branch
carries no merge commits.

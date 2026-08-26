# Public-estate normalization criteria

The doctrine every public `truvity/*` repository, artifact, and tool is
held to. Tickets and reviews reference the codes (e.g. "violates N1,
B6") instead of restating the rules. Umbrella ticket: INF-584.

Each criterion carries a one-line check — the command or observation
that settles whether a repo conforms, so an audit is mechanical.

## R — Repository shape

- **R1 — One public MIT repo per platform concern.** Chart + binary(s)
  + client are versioned together: one tag releases the united set, so
  any released (chart, image) pair is self-consistent by construction.
  *Check: does a single `v*` tag stamp every artifact the repo ships?*
- **R2 — Fresh git history on extraction.** Code extracted from a
  private repo is re-initialized, not history-swept; the leak canary
  runs over whatever history goes public.
  *Check: first commit predates nothing private; canary run recorded in
  the extraction PR.*
- **R3 — devbox + Justfile toolchain.** Anything operable documents
  itself: README plus architecture / day-1 / day-2 docs.
  *Check: `devbox.json`, `Justfile`, `docs/` exist and are current.*

## N — Artifacts and naming

- **N1 — Images nest: `ghcr.io/truvity/<repo>/<artifact>`.** The
  artifact segment is a **role name** (`runner`, `operator`, `updater`,
  `server`, `mapper`, `broker`, `sync`) — never a repo-name repeat
  (`gemaal/gemaal` is wrong).
  *Check: `gh api` the package slug; confirm two segments under the
  org, second segment a role word.*
- **N2 — Charts live flat under `ghcr.io/truvity/charts/<name>`.** The
  OCI tag is the chart version, bare semver. Charts do not nest — the
  `charts/` namespace is the estate-wide chart registry.
  *Check: `helm show chart oci://ghcr.io/truvity/charts/<name>`.*
- **N3 — Committed `Chart.yaml` version is `0.0.0-dev`.** The release
  stamps the real version; a committed real value is dead weight that
  reads as truth and drifts.
  *Check: `grep '^version:' charts/*/Chart.yaml` says `0.0.0-dev`.*

## B — Build and release

- **B1 — Images via goreleaser ko, distroless by default.** A
  Dockerfile + buildx build needs a written justification (ci-plane's
  runner: a fat image is the product). Always set `ko-docker-repo`
  explicitly. **Never rely on ko's `repositories:` key — it is inert**
  when `base_import_paths: true`: ko names the image after the cmd
  directory, full stop. (Proven by the github-roster acceptance image:
  the key claimed `github-roster-acceptance`, ko published
  `roster-acceptance`, and the chart's test pod pulled a name that
  did not exist.)
  *Check: `.goreleaser.yaml` has `kos:` with no `repositories:`; the
  workflow passes `ko-docker-repo: ghcr.io/truvity/<repo>`.*
- **B2 — Charts are packaged and pushed by helmctl** (from
  `truvity/ocictl`), not `helm package` + `helm push`: the tgz is
  repacked deterministically (epoch timestamps, sorted entries, zeroed
  owners) and the OCI manifest carries no `created` annotation —
  identical content ⇒ identical registry digest.
  *Check: release the same chart content twice; the chart digests in
  ghcr are equal.*
- **B3 — Charts pin their own images by digest.** Values follow the
  `images.<component>.{registry,repository,tag,digest}` convention with
  a helper template that `fail`s on a missing digest; releases run
  `helmctl package --manifest --require-image-digests` over the
  goreleaser dist manifest.
  *Check: `values.yaml` images carry `digest:`; the release log shows
  `--require-image-digests`.*
- **B4 — Releases go through the shared `release-public.yaml`.** A
  hand-rolled release workflow needs a written reason. `v*` tags are
  cut on master only, protected by a tag ruleset whose bypass is the CI
  automation App; weekly auto-release cuts the next patch when master
  is ahead; renovate automerges security/patch bumps.
  *Check: `release.yaml` is a thin caller; tag ruleset lists the App;
  `auto-release.yaml` gated on `AUTO_RELEASE`.*
- **B5 — Promotion is pull-based.** Producers just release. Consumers
  (gitops) pull versions via renovate-annotated pins behind render +
  golden gates; the pin PR is the deploy act. No workload deploys from
  a repo path or a floating ref.
  *Check: no `bump-pin` jobs in producers; no `targetRevision: master`
  + `path:` Applications for versioned components.*
- **B6 — The shared-workflow pin in every repo is renovate-managed.**
  Pin drift across the estate is a defect, not a preference. The same
  applies to any annotated tool pin (e.g. `HELMCTL_VERSION`).
  *Check: `uses: truvity/ci-workflows/...@<sha> # vX.Y.Z` matches the
  latest tag, or renovate has an open PR for it.*

## P — Portability

- **P1 — Charts are AWS-auth-agnostic.** Workloads use the SDK default
  chain; every chart exposes
  `serviceAccount.{create,name,annotations}`; both EKS Pod Identity and
  IRSA are documented in the chart README (IRSA is the path for
  non-EKS clusters via amazon-eks-pod-identity-webhook).
  *Check: `helm template` with
  `serviceAccount.annotations."eks\.amazonaws\.com/role-arn"` renders
  the annotation.*
- **P2 — No Truvity-shaped defaults in values.** Regions, account IDs,
  ECR registry URLs, and SSM prefixes live in gitops values; anything
  Truvity-specific in a public repo is clearly marked as an example.
  *Check: `grep -rE '[0-9]{12}\.dkr\.ecr|eu-central-1' charts/` hits
  only comments marked as examples.*
- **P3 — Schemas are strict but never closed over extension points.**
  `values.schema.json` validates shapes; it must not set
  `additionalProperties: false` on blocks users legitimately extend
  (serviceAccount annotations, pod annotations, labels). (Proven by
  google-group-sync: a closed serviceAccount schema made attaching any
  annotation a hard fork.)
  *Check: extension-point blocks accept arbitrary string maps.*

## Applying the doctrine

A conforming repo is boring: one tag releases role-named nested images
and a deterministic chart, renovate keeps every pin current, gitops
pulls the release through an annotated pin, and an outside operator on
plain EKS — or a non-EKS cluster — installs the chart without forking
anything. Deviations are fine when written down; the criteria exist so
the write-down has something to point at.

# Development commands. Tools come from devbox.json (`devbox shell`, or
# direnv). CI runs `lint`, `golden`, `schema` and `leak-canary` as recipes
# of the shared check workflow (truvity/ci-workflows), each as its own job;
# the chart assertions, the image builds and the Go jobs are still ci.yaml's
# own jobs.

charts := "ci-builders"
modules := "image/nix-worker-client image/nix-worker-host-cert image/runner-watchdog image/cache-redirect deploy/pulumi/runneridentity"

# Lint the chart, from the CI base values (it does not render bare: it
# refuses to guess the estate facts it needs), and the shell scripts'
# syntax.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    for c in {{ charts }}; do
        helm lint "charts/$c" -f "tests/ci/$c.yaml"
    done
    bash -n image/*.sh hack/*.sh

# Render every tests/cases/<chart>/<case>/values.yaml and compare it with
# tests/golden/<chart>/<case>.yaml; prove every tests/invalid/<chart>/
# fixture is refused, for the reason its first line names.
golden:
    bash hack/golden.sh

# Rewrite the goldens after a template change. Review the diff.
golden-update:
    bash hack/golden.sh update

# Schema-validate the golden renders (kubeconform -strict against the
# Kubernetes API): the goldens compare text, this proves the API server
# would accept what they say.
schema:
    bash hack/schema.sh

# The reason this repository can be public.
leak-canary:
    bash hack/leak-canary.sh

# The helper modules under image/ and the Pulumi package under deploy/,
# each its own Go module.
test:
    #!/usr/bin/env bash
    set -euo pipefail
    for m in {{ modules }}; do
        (cd "$m" && test -z "$(gofmt -l .)" && go vet ./... && go test -count=1 ./...)
    done

# Reachable Go advisories in the helper modules. Runs from security.yaml,
# daily and on every change, and is deliberately NOT part of `check`: an
# advisory published overnight must not turn every pull request red.
vuln:
    #!/usr/bin/env bash
    set -euo pipefail
    for m in {{ modules }}; do
        (cd "$m" && govulncheck ./...)
    done

# The pull-request gate that runs on a laptop.
check: lint golden schema leak-canary

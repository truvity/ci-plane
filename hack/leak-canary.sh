#!/usr/bin/env bash
# This repository is public and its history cannot be unpublished — a
# rewrite changes the SHAs but not what was already fetched. So the rule
# ("mechanism only; particulars are caller inputs or org variables") is
# enforced mechanically rather than remembered.
#
# Add a pattern here the first time something new turns out to be a
# particular. Never add an exception without one.
#
# Copied from truvity/ci-workflows (v3.13.1). What this copy adds, each with
# its reason: tracker keys, and an ALLOW regex per pattern for the shapes
# that are mechanism rather than particulars (see `allows` below).
set -uo pipefail

# The 12-digit patterns are anchored on word boundaries. Without them,
# `[0-9]{12}` also matches a 12-digit run that happens to fall inside a
# longer hex string -- and a nixpkgs commit SHA is exactly that. The
# devbox bump to 17de0b976395537756f30a3e78f2f06e5cec89ed contains
# `976395537756`, which failed this canary simultaneously in every repo
# that carries it, for a value that is neither a particular nor secret.
# `\b` keeps every real shape (bare, in an ARN, as an ECR host: each is
# bounded by a non-word character) and drops the hex-embedded ones.
patterns=(
  '\b[0-9]{12}\b'                          # AWS account id
  'arn:aws'                            # any ARN
  '\b[0-9]{12}\.dkr\.ecr\.'              # ECR registry host
  '\.svc\.cluster\.local'              # in-cluster DNS
  '/secrets/'                          # SSM parameter paths
  'truvity-[a-z0-9-]*-(ci-cache|artifacts|state)'   # S3 buckets
  '\.truvity\.(xyz|com|co)'            # internal hostnames
  'glpat-|ghp_|github_pat_'            # tokens, in case of an accident
  '\b(INF|SQU)-[0-9]+'                  # tracker keys: they point at a board
                                       # nobody outside can open
)

# What a hit may look like and still be mechanism, one regex per pattern
# above (same order; empty allows nothing). Matched against the whole
# `file:line:text` hit, so an allowance can name its file.
allows=(
  '123456789012'                       # the documented placeholder account
  ''
  '123456789012\.dkr\.ecr\.'           # the documented placeholder registry
  # A templated or placeholder namespace is how the charts NAME a Service,
  # not which one; kube-dns in kube-system is Kubernetes' own, not an
  # estate's. The two image ARGs are known debt: the images still bake one
  # estate's in-cluster Nix substituter by default. v4.1.0 adds a chart
  # value (arc-runners' nixCache.url) that overrides it without a rebuild,
  # but un-baking the DEFAULT itself is a separate, coordinated change for
  # a later release, once every consumer has set nixCache.url explicitly.
  '(%s|example|<namespace>|<ns>)\.svc\.cluster\.local|kube-dns\.kube-system\.svc\.cluster\.local|^image/(runner|nix-worker)/Dockerfile:[0-9]+:ARG NIX_SUBSTITUTERS='
  # Where Kubernetes mounts a projected token, not a parameter path.
  '/var/run/secrets/'
  ''
  ''
  ''
  ''
)

if [ "${#patterns[@]}" -ne "${#allows[@]}" ]; then
  echo "leak-canary.sh: ${#patterns[@]} patterns but ${#allows[@]} allowances" >&2
  exit 2
fi

fail=0

# Scan TRACKED FILES ONLY. The point of this canary is to stop particulars
# being committed, so git's index is exactly the right scope -- and a
# recursive walk of the working tree is not. It descended into generated,
# gitignored directories: .devbox/state.json carries a
# `nix_print_dev_env_hash` whose hex contains a 12-digit run, which matched
# the AWS-account-id pattern. That made the canary fail on a clean checkout
# for a value that is neither committed nor secret.
#
# This matters more than a nuisance: a canary that cries wolf is one people
# learn to skip, and this one is what stands between us and publishing
# particulars from a public repo.
mapfile -d '' tracked < <(git ls-files -z)

for i in "${!patterns[@]}"; do
  p=${patterns[$i]}
  allow=${allows[$i]}
  # Exclude this script: it necessarily contains the patterns it bans.
  hits=$(printf '%s\0' "${tracked[@]}" \
           | grep -zZv '^hack/leak-canary\.sh$' \
           | xargs -0 -r grep -InE "$p" 2>/dev/null)
  if [ -n "$allow" ] && [ -n "$hits" ]; then
    hits=$(grep -vE "$allow" <<< "$hits")
  fi
  if [ -n "$hits" ]; then
    echo "LEAK: pattern /$p/ matched — particulars belong in caller inputs or org variables:"
    echo "$hits" | head -5 | sed 's/^/    /'
    fail=1
  fi
done

if [ "$fail" = 0 ]; then
  echo "leak canary clean — ${#patterns[@]} patterns checked, no particulars found"
fi
exit $fail

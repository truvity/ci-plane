# The RELEASE image for cache-redirect.
#
# IT CONTAINS NO `RUN`, AND THAT IS THE WHOLE POINT.
#
# Every instruction that has to EXECUTE lives in
# image/cache-redirect/Dockerfile, which is built once per architecture on a
# NATIVE runner. This file only assembles: `FROM` a manifest list plus
# labels, which buildx resolves per platform without emulating anything --
# the same split as the runner and nix-worker images, and the same guard
# (hack/final-images-are-assembly.sh).
ARG BASE_IMAGE=ghcr.io/truvity/ci-plane/cache-redirect-base
ARG BASE_TAG=latest

FROM ${BASE_IMAGE}:${BASE_TAG}

# OCI metadata belongs here rather than in the base: it names the
# RELEASE, and the base is built before the release exists.
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="cache-redirect" \
      org.opencontainers.image.description="Pull-through cache for Nix and Go downloads that redirects hits to presigned object-store URLs" \
      org.opencontainers.image.source="https://github.com/truvity/ci-plane" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0"

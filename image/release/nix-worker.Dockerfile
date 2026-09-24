# The RELEASE image for the Nix remote worker.
#
# IT CONTAINS NO `RUN`, AND THAT IS THE WHOLE POINT.
#
# Every instruction that has to EXECUTE lives in image/nix-worker/Dockerfile,
# which is built once per architecture on a NATIVE runner. This file only
# assembles: `FROM` a manifest list plus labels, which buildx resolves per
# platform without emulating anything. Measured on the same image: a
# RUN-less arm64 stage is "DONE 0.0s"; a single `apk add` under QEMU is
# two seconds, and this image's real work is a nix install, a devbox
# install, apt and the docker toolchain.
#
# So: adding ONE `RUN` here silently reintroduces QEMU for the whole
# arm64 half of every release. hack/final-images-are-assembly.sh refuses
# that, and it is not a style rule -- it is the property this split
# exists to buy.
#
# Why a build at all, rather than re-tagging the base? Because goreleaser
# has to be the thing that produces the released image: it writes
# dist/artifacts.json, and helmctl reads THAT to fill the charts' image
# values. A digest the charts get from anywhere else is a digest that can
# be stale, which is the failure release-public.yaml was written to make
# impossible ("it rendered, it pushed, the run was green, and the chart
# could not install").
ARG BASE_IMAGE=ghcr.io/truvity/ci-plane/nix-worker-base
ARG BASE_TAG=latest

FROM ${BASE_IMAGE}:${BASE_TAG}

# OCI metadata belongs here rather than in the base: it names the
# RELEASE, and the base is built before the release exists.
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="the Nix remote worker" \
      org.opencontainers.image.source="https://github.com/truvity/ci-plane" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0"

# cache-redirect: the pull-through cache that answers a hit on a large
# immutable download (a Nix NAR, a Go module zip) with a redirect to a
# presigned URL of its bucket (charts/ci-builders `cacheRedirect`; see
# docs/architecture.md, "cache-redirect").
#
# Built per architecture by .github/workflows/release.yaml like the other
# bases, and assembled into the release image by
# image/release/cache-redirect.Dockerfile, which executes nothing. A static
# binary: CGO is off, so it cross-compiles on the build platform and the
# final stage has no userland to run anything in.

# renovate: datasource=docker depName=golang
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETARCH
COPY cache-redirect/ /src/cache-redirect/
RUN cd /src/cache-redirect \
 && go mod download \
 && CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/cache-redirect .

# distroless static: CA certificates for the upstreams and the store, and
# nothing else. nonroot is uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cache-redirect /usr/local/bin/cache-redirect
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cache-redirect"]

# Multi-stage build mirroring hive's pattern: the git hash is stamped into
# the binary via ldflags so `dibs --version` prints the running commit
# (freshness-probe friendly).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:eb61c3873c97416fff0116553fff65d67a3549f72e11d9758849a689cdd8b523 AS builder
RUN apk add --no-cache git
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CI passes GIT_HASH explicitly (checkouts are often detached); local builds
# fall back to the working tree.
ARG GIT_HASH=unknown
ARG TARGETOS TARGETARCH
RUN GH="$GIT_HASH" && \
    if [ "$GH" = "unknown" ] || [ -z "$GH" ]; then GH=$(git rev-parse HEAD 2>/dev/null || echo "unknown"); fi && \
    GS=$(echo "$GH" | cut -c1-7) && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -ldflags "-X main.gitHash=${GH} -X main.gitShort=${GS}" -o /dibs ./cmd/dibs

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /dibs /usr/local/bin/dibs
# JSON idea store + repo registry live here; mount a volume in production.
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["dibs"]

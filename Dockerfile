# syntax=docker/dockerfile:1

# ---- build ----
# Go 1.26 is the floor: github.com/google/go-github/v92 requires it.
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO_ENABLED=0 produces a static binary, which is what lets the final stage be
# distroless static. -trimpath keeps build paths out of the binary.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/shuriken \
      ./cmd/shuriken

# ---- runtime ----
# Static distroless: no shell, no package manager, no libc. If the process is
# ever compromised there is nothing in the image to pivot with.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/shuriken /usr/local/bin/shuriken

# nonroot is uid 65532 in the distroless images.
USER nonroot:nonroot
EXPOSE 8080

# No shell in the image, so this is the exec form by necessity as well as
# preference.
ENTRYPOINT ["/usr/local/bin/shuriken"]

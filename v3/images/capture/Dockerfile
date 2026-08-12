# syntax=docker/dockerfile:1

FROM docker.io/library/golang:1.26.5@sha256:7caba5286b4c3613a337b709c573047d8ae62ee76106647313b61e72b99f20af AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src/v3
COPY v3/go.mod v3/go.sum ./
COPY v3/api ./api
COPY v3/cmd ./cmd
COPY v3/internal ./internal
COPY v3/pkg ./pkg

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o /out/agw-capture ./cmd/agw-capture

FROM docker.io/library/debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

RUN apt-get update \
    && apt-get install --no-install-recommends --yes ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 1000 agw \
    && useradd --uid 1000 --gid 1000 --no-create-home --shell /usr/sbin/nologin agw

COPY --from=build /out/agw-capture /agw/capture

USER 1000:1000
WORKDIR /workspace/repo
ENTRYPOINT ["/agw/capture"]

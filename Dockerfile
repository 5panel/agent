# Build a static binary and ship it on a distroless base: no shell, no
# package manager, nothing but the agent. Configure through the
# environment (FIVEPANEL_TOKEN, DATABASE_URL, ...) or mount a config.yaml
# and pass `run --config /etc/fivepanel/config.yaml`.
#
#   docker build -t ghcr.io/5panel/agent .
#   docker run --rm --network host \
#     -e FIVEPANEL_TOKEN=fp_live_... \
#     -e DATABASE_URL=mysql://fivepanel_ro:pass@127.0.0.1:3306/qbcore \
#     ghcr.io/5panel/agent

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/5panel/agent/internal/version.Version=${VERSION} -X github.com/5panel/agent/internal/version.Commit=${COMMIT}" \
    -o /out/fivepanel-agent ./cmd/fivepanel-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fivepanel-agent /fivepanel-agent
USER nonroot:nonroot
ENTRYPOINT ["/fivepanel-agent"]
CMD ["run"]

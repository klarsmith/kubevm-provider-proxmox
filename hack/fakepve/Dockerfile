# In-cluster fake Proxmox for testing the manager's deployment (RBAC,
# Secrets, probes) on kind without a real PVE. Build from the repo root:
#   docker build -f hack/fakepve/Dockerfile -t fakepve:dev .
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/fakepve/ cmd/fakepve/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /fakepve ./cmd/fakepve

FROM gcr.io/distroless/static:nonroot
COPY --from=build /fakepve /fakepve
USER 65532:65532
ENTRYPOINT ["/fakepve", "--listen", "0.0.0.0:8006"]

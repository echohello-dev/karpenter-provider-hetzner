# syntax=docker/dockerfile:1.7
FROM golang:1.26 AS build
WORKDIR /src

# Cache Go module downloads as a separate layer.
COPY go.mod go.sum ./
RUN go mod download -x

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=" -o /out/karpenter-provider-hetzner ./cmd/controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/karpenter-provider-hetzner /bin/karpenter-provider-hetzner
USER 65532:65532
ENTRYPOINT ["/bin/karpenter-provider-hetzner"]

EXPOSE 8080 8081 9443

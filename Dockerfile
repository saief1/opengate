# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/opengate-dev/opengate/internal/server.Version=${VERSION}" -o /out/opengate ./cmd/opengate

# Distroless static: no shell, no package manager, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/opengate /usr/local/bin/opengate
ENV OPENGATE_LISTEN=0.0.0.0:8080 \
    OPENGATE_DATABASE=/data/opengate.db
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/usr/local/bin/opengate", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/opengate"]
CMD ["serve"]

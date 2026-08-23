# go.mod declares go 1.25: the runtime dependencies are all pinned to their
# newest Go 1.23-compatible releases, but testcontainers-go — test-only, never
# in this binary — drags a docker/otel closure that no longer builds under 1.23.
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
# Two binaries out of one build stage: one image, one module download, two
# processes. The closerd shares every dependency the auctiond already has.
RUN CGO_ENABLED=0 go build -trimpath -o /out/auctiond ./cmd/auctiond \
 && CGO_ENABLED=0 go build -trimpath -o /out/closerd ./cmd/closerd

FROM alpine:3.20
RUN adduser -D -u 10001 auction
USER auction
COPY --from=build /out/auctiond /usr/local/bin/auctiond
COPY --from=build /out/closerd /usr/local/bin/closerd
EXPOSE 8080 8081
# The default is the API; the closerd service overrides the entrypoint.
ENTRYPOINT ["/usr/local/bin/auctiond"]

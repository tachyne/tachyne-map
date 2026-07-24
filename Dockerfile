# Build the static map-pod binary (three.js viewer embedded via go:embed) and
# ship it on distroless/static, which carries CA certificates — the pod fetches
# Mojang's client jar over HTTPS at runtime, so a bare scratch base would fail
# TLS. vet + tests run in the build so a broken commit never ships an image.
FROM golang:1.26-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go vet ./... && CGO_ENABLED=0 go test ./...
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tachyne-map .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/tachyne-map /tachyne-map
ENV MAP_CACHE=/cache
EXPOSE 8100
ENTRYPOINT ["/tachyne-map"]

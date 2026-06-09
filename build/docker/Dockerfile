# Multi-stage build of the relay-capable internetmerge binary (CGO-free, static).
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/kaandikec/internetmerge/internal/version.Version=${VERSION}" \
    -o /internetmerge ./cmd/internetmerge

FROM gcr.io/distroless/static-debian12
COPY --from=build /internetmerge /internetmerge
EXPOSE 7000
ENTRYPOINT ["/internetmerge"]
CMD ["relay", "--listen", ":7000"]

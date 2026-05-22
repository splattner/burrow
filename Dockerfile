FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.25.10 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/burrow ./cmd/root

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/burrow /burrow

ENTRYPOINT ["/burrow"]

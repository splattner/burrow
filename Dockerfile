FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.25.10 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/k8s-reverse-tunnel ./cmd/root

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/k8s-reverse-tunnel /k8s-reverse-tunnel

ENTRYPOINT ["/k8s-reverse-tunnel"]

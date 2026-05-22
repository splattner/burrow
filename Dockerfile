FROM golang:1.22 AS builder
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/k8s-reverse-tunnel ./cmd/root

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/k8s-reverse-tunnel /k8s-reverse-tunnel

ENTRYPOINT ["/k8s-reverse-tunnel"]

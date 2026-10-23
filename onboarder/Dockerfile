FROM golang:1.27-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /app/onboarder .

# distroless/static carries the system CA bundle, for an Ops certificate from
# a public CA; a private CA comes in through the ops_ca value.
FROM gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.source=https://github.com/warroyo/platform-observability-addon

COPY --from=builder /app/onboarder /onboarder
USER 65532:65532

ENTRYPOINT ["/onboarder"]

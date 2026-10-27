# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w' -o /out/mock-llm ./cmd/mock-llm

FROM alpine:3.22
COPY --from=build /out/mock-llm /usr/local/bin/mock-llm
COPY config/default.json /etc/mock-llm/config.json
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 CMD ["/usr/local/bin/mock-llm", "--healthcheck", "http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["/usr/local/bin/mock-llm"]
CMD ["--config", "/etc/mock-llm/config.json"]

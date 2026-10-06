# Builds cmd/registry-proxy as a static binary and ships it on a minimal,
# non-root, distroless runtime image. Both base images are pinned by digest,
# not tag: re-resolve deliberately and update these lines explicitly when a
# base image needs to move.
FROM golang@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=build-ca,required=false \
    if [ -f /run/secrets/build-ca ]; then export SSL_CERT_FILE=/run/secrets/build-ca; fi \
    && go mod download
COPY cmd/registry-proxy ./cmd/registry-proxy
COPY internal/registryproxy ./internal/registryproxy
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/registry-proxy ./cmd/registry-proxy

FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/registry-proxy /registry-proxy
USER 65532:65532
ENTRYPOINT ["/registry-proxy"]

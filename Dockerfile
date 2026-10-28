FROM golang:1.27.3-trixie@sha256:7b072c590a326b9d2a43b165ae247f6e8eafff32e1ad154ab97139e89f053be9 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN APP_VERSION="$(tr -d '\r\n' < VERSION)" && \
    printf '%s\n' "$APP_VERSION" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' && \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X knowledge-ba46-flow/internal/version.Version=${APP_VERSION}" \
      -o /out/KBM-flow ./cmd/KBM-flow

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
USER nonroot
WORKDIR /app
COPY --from=build /out/KBM-flow /usr/local/bin/KBM-flow
COPY prompts /app/prompts
COPY LICENSE THIRD-PARTY-NOTICES.md /usr/share/licenses/knowledge-ba46-flow/
ENTRYPOINT ["/usr/local/bin/KBM-flow"]

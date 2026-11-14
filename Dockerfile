FROM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN APP_VERSION="$(tr -d '\r\n' < VERSION)" && \
    printf '%s\n' "$APP_VERSION" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' && \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X knowledge-ba46-flow/internal/version.Version=${APP_VERSION}" \
      -o /out/KBM-flow ./cmd/KBM-flow

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
USER nonroot
WORKDIR /app
COPY --from=build /out/KBM-flow /usr/local/bin/KBM-flow
COPY prompts /app/prompts
COPY LICENSE THIRD-PARTY-NOTICES.md /usr/share/licenses/knowledge-ba46-flow/
ENTRYPOINT ["/usr/local/bin/KBM-flow"]

FROM golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN APP_VERSION="$(tr -d '\r\n' < VERSION)" && \
    printf '%s\n' "$APP_VERSION" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' && \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X knowledge-ba46-flow/internal/version.Version=${APP_VERSION}" \
      -o /out/KBM-flow ./cmd/KBM-flow

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
USER nonroot
WORKDIR /app
COPY --from=build /out/KBM-flow /usr/local/bin/KBM-flow
COPY prompts /app/prompts
COPY LICENSE THIRD-PARTY-NOTICES.md /usr/share/licenses/knowledge-ba46-flow/
ENTRYPOINT ["/usr/local/bin/KBM-flow"]

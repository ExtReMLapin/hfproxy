ARG GO_VERSION=1.27.3
ARG BUILD_COMMIT=unknown
FROM golang:${GO_VERSION}-alpine AS build

RUN apk add --no-cache ca-certificates git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=1.0.0 -X main.commit=${BUILD_COMMIT}" -o /out/hfproxy ./cmd/hfproxy

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 65532 hfproxy \
    && adduser -S -D -H -u 65532 -G hfproxy hfproxy
COPY --from=build /out/hfproxy /usr/local/bin/hfproxy
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/hfproxy"]
CMD ["serve"]

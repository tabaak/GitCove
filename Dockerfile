# The build stage runs on the builder's own architecture and cross-compiles, so multi-arch builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -X main.version=$VERSION" -o /cove ./cmd/cove

FROM alpine:3.22
# GitCove serves repos through git-http-backend, which Alpine packages in git-daemon.
RUN apk add --no-cache git git-daemon ca-certificates && adduser -D -u 10001 cove && mkdir /data && chown cove /data
COPY --from=build /cove /usr/local/bin/cove
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/licenses/gitcove/
USER cove
ENV COVE_DATA=/data
VOLUME /data
EXPOSE 8418
ENTRYPOINT ["cove"]
CMD ["serve"]

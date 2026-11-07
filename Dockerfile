FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /cove ./cmd/cove

FROM alpine:3.22
# git provides git-http-backend, which GitCove runs to serve repos.
RUN apk add --no-cache git ca-certificates && adduser -D -u 10001 cove && mkdir /data && chown cove /data
COPY --from=build /cove /usr/local/bin/cove
USER cove
ENV COVE_DATA=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["cove"]
CMD ["serve"]

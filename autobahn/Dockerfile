# Builds the gowest Autobahn echo server. Context is the repository root (see
# docker-compose.yml), so the whole module is available to `go build`.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
# No external dependencies, so there is nothing to download; copy the source and
# build the static echo server.
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/autobahn-server ./autobahn

FROM alpine:3.20
COPY --from=build /bin/autobahn-server /bin/autobahn-server
EXPOSE 9001
ENTRYPOINT ["/bin/autobahn-server", "-addr", ":9001"]

# ---- build (latest Go + latest surf/g) ----
FROM golang:latest AS build
WORKDIR /src
COPY main.go .
RUN go mod init toyzz-watcher \
    && go get github.com/enetx/surf@latest github.com/enetx/g@latest \
    && go mod tidy \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /watcher .

# ---- runtime (tiny distroless image, CA certs included) ----
FROM gcr.io/distroless/static-debian12
COPY --from=build /watcher /watcher
ENV STATE_FILE=/data/state.json
VOLUME ["/data"]
ENTRYPOINT ["/watcher"]

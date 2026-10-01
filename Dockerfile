# The controller with its runtime (FreeRDP 3, Xvfb, xdotool, ImageMagick), for
# hosts that cannot run it natively — it is Linux-only, so this is how to use it
# from macOS or Windows. The Windows agent is built alongside it, so pushing the
# agent works from the container too.
#
#   docker build -t win-rdp-mcp .
#   docker run -i --rm -v "$PWD/target.pass:/run/secrets/target.pass:ro" win-rdp-mcp \
#     control --target 192.168.1.50 --user administrator --pass-file /run/secrets/target.pass

FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/win-rdp-mcp . \
 && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/win-rdp-mcp.exe .

FROM debian:trixie-slim
# x11-utils provides xdpyinfo, the controller's display readiness probe.
# netbase provides /etc/services, which FreeRDP's HTTPS client needs to resolve
# the `https` port for the AAD discovery fetch (`/sec:aad`); without it that
# lookup fails with HTTP_STATUS_UNKNOWN.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      freerdp3-x11 xvfb x11-utils xdotool imagemagick ca-certificates netbase \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/win-rdp-mcp"]

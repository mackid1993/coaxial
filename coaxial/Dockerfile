# Coaxial: one Go program on top of the stock gluetun image (the VPN). Standard library only, apart from
# pro-bing, which pings the VPN servers (see ACKNOWLEDGMENTS.md). Built in the
# stock Go image. The program is the container's first process: it starts gluetun's own entrypoint with the
# VPN settings from /config/settings.json, and the proxy once the VPN is up.
FROM golang:alpine AS build
WORKDIR /src
COPY go.mod go.sum *.go VERSION bump-version.sh ./
COPY ui ./ui
# The version is the build time (UTC): vYYYY.MM.DD.HHMM. The workflow passes one stamp to every
# architecture as STAMP; a build made by hand stamps itself.
ARG STAMP=""
RUN if [ -n "$STAMP" ]; then printf '%s\n' "$STAMP" > VERSION; else sh bump-version.sh; fi
RUN CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local go vet ./... && \
    CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local go build -ldflags="-s -w" -o /coaxial .

FROM qmcgaw/gluetun:latest
COPY --from=build /coaxial /usr/local/bin/coaxial
EXPOSE 8095 8096 8195
HEALTHCHECK --interval=10s --timeout=10s --start-period=30s --retries=3 CMD ["/usr/local/bin/coaxial", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/coaxial"]

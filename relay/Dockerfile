# The relay: one Go program, standard library only. Built in the stock Go image, run in stock Alpine (its
# shell picks the providers out of .env at start, and its wget and ip run the health check).
FROM golang:alpine AS build
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local go vet ./... && \
    CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local go build -ldflags="-s -w" -o /relay .

FROM alpine:latest
RUN apk upgrade --no-cache
COPY --from=build /relay /usr/local/bin/relay
USER 65534:65534
EXPOSE 8197
ENTRYPOINT []
CMD ["/usr/local/bin/relay"]

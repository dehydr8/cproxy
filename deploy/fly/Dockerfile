# cproxy serve image. Build context: the repository root
# (fly deploy picks this up from deploy/fly/fly.toml).
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/dehydr8/cproxy/internal/version.Version=${VERSION}" \
      -o /out/cproxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cproxy /cproxy
# Configuration comes from the environment (fly secrets); nothing is written.
ENV CPROXY_HOME=/tmp/cproxy
EXPOSE 7788
ENTRYPOINT ["/cproxy"]
CMD ["serve", "--addr", ":7788"]

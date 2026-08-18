# Kerberon builds to a static binary with no CGO, so the runtime image needs
# nothing but the binary itself. There is no libc to match, no package manager,
# and nothing to patch for CVEs that only exist in a base image.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /kerberon ./cmd/kerberon

FROM scratch
# The timezone database is compiled into the binary, so scratch is genuinely
# enough. Certificates are not: outbound HTTPS to ntfy or Telegram needs a root
# store, and scratch has none.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /kerberon /kerberon

# Runs unprivileged. Nothing here needs root, and a pager is an unusual thing
# to hand extra authority to.
USER 65534:65534
EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/kerberon"]
CMD ["serve", "--config", "/etc/kerberon/kerberon.yaml"]

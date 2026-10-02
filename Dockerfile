# 1st stage, build app
FROM golang:1.27.1-trixie AS builder
WORKDIR /build/app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -mod=readonly -ldflags "-s -w" -trimpath -o tenderduty .

# 2nd stage, create a user and install certificates for upstream TLS servers.
FROM debian:13-slim AS ssl
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates adduser && \
    addgroup --gid 26657 --system tenderduty && adduser --uid 26657 --ingroup tenderduty --system --home /var/lib/tenderduty tenderduty && \
    install -d -m 0750 -o tenderduty -g tenderduty /var/lib/tenderduty

# 3rd and final stage, copy the static binary, certificates, and service user.
FROM scratch
COPY --from=ssl /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=ssl /etc/passwd /etc/passwd
COPY --from=ssl /etc/group /etc/group
COPY --from=ssl --chown=tenderduty:tenderduty /var/lib/tenderduty /var/lib/tenderduty

COPY --from=builder /build/app/tenderduty /bin/tenderduty
COPY --from=builder /build/app/example-config.yml /var/lib/tenderduty

USER tenderduty
WORKDIR /var/lib/tenderduty

ENTRYPOINT ["/bin/tenderduty"]

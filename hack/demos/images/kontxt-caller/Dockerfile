FROM alpine:3.20

RUN apk add --no-cache curl jq bash ca-certificates \
    && update-ca-certificates

COPY caller.sh /usr/local/bin/caller.sh
RUN chmod +x /usr/local/bin/caller.sh

# All output is piped through a JWT-prefix redactor (sed) before printing.
# See caller.sh for the redaction contract.
ENTRYPOINT ["/usr/local/bin/caller.sh"]

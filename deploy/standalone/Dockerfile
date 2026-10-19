FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY pmcluster edge /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/pmcluster"]

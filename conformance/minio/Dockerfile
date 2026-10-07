FROM busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e AS tools
FROM scratch
COPY --from=tools /bin/busybox /usr/bin/mkdir
COPY minio /usr/bin/minio
COPY tmp /tmp
ENV PATH=/usr/bin HOME=/tmp
ENTRYPOINT ["/usr/bin/minio"]

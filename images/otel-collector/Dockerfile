# Project-owned OTel Collector image.
#
# Why we repackage instead of using otel/opentelemetry-collector-contrib
# directly: the upstream distroless images ship a dynamically-linked binary but
# (at least on the arm64 variant of 0.116.0) omit the glibc loader
# /lib/ld-linux-<arch>.so.1, so the binary fails with
# "exec /otelcol-contrib: no such file or directory". debian:12-slim provides
# the loader, so the same binary runs on both arm64 (local) and amd64 (CI).
#
# Why CONTRIB (not core): M11's trace-redaction seam (§13.3) is a `transform`
# processor using OTTL replace_pattern — surgical, per-detector regex redaction
# of the sensitive payload attributes before export. `transform` lives in the
# contrib distribution only; core carries attributes/filter/batch but no
# substring-level regex redaction. The full traces pipeline used
# (otlp / batch / transform / debug / otlphttp) is all in contrib.
FROM otel/opentelemetry-collector-contrib:0.116.0 AS upstream

FROM debian:12-slim
COPY --from=upstream /otelcol-contrib /otelcol-contrib
# distroless upstream ships default config at /etc/otelcol-contrib/config.yaml;
# the controller mounts ours at /etc/otel/config.yaml and passes --config.
ENTRYPOINT ["/otelcol-contrib"]

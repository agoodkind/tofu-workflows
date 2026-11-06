FROM docker.io/library/golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS build
ENV GOOS=linux GOARCH=amd64 CGO_ENABLED=0
WORKDIR /src
COPY . .
RUN make build

FROM docker.io/library/debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=ghcr.io/opentofu/opentofu:1.13.2@sha256:912846109c72b5d0ed514653648e6690d0b52f831766561fa605bd6d662cd1b0 /usr/local/bin/tofu /usr/local/bin/tofu
ADD --checksum=sha256:03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc --chmod=0755 https://github.com/cloudflare/cloudflared/releases/download/2026.9.1/cloudflared-linux-amd64 /usr/local/bin/cloudflared
COPY --from=build /src/dist/tofu-workflows /usr/local/bin/tofu-workflows
ENTRYPOINT ["/usr/local/bin/tofu-workflows"]

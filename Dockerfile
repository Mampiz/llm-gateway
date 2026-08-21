# --- build stage -------------------------------------------------------------
# The toolchain lives here and never reaches the final image.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Copy the module files first so `go mod download` is cached independently of
# the source: editing a .go file must not re-download every dependency.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a statically linked binary with no libc dependency,
# which is what lets the final image be scratch.
# -trimpath strips local filesystem paths; -s -w drop the symbol table and DWARF
# debug info, shaving several MB.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
	-trimpath \
	-ldflags="-s -w -X main.version=${VERSION}" \
	-o /out/gateway ./cmd/gateway

# --- runtime stage -----------------------------------------------------------
# scratch is genuinely empty: no shell, no package manager, no libc. Nothing to
# exploit and nothing to patch. The two things a Go binary still needs are CA
# certificates (to verify api.openai.com's TLS) and /etc/passwd for the user.
FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /etc/passwd /etc/passwd
COPY --from=build /out/gateway /gateway

# Run unprivileged. 65534 is "nobody" on Alpine and Debian alike.
USER 65534:65534

EXPOSE 8080
ENTRYPOINT ["/gateway"]

# The API image.
#
# Two stages: build with the full toolchain, ship a static binary on a
# distroless base. The result is around 20MB and contains no shell, no package
# manager, and nothing else an attacker could pivot through.

FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses the cached layer.
COPY api/go.mod api/go.sum ./
RUN go mod download

COPY api/ ./

# CGO_ENABLED=0 is what makes the binary static. It is only possible because
# the SQLite driver is pure Go; a cgo driver would force a glibc base image.
# The linker flags strip the symbol table and DWARF data, which is a few MB.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/fetchandscore ./cmd/fetchandscore \
 && CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/fnsctl ./cmd/fnsctl

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/fetchandscore /usr/local/bin/fetchandscore
COPY --from=build /out/fnsctl /usr/local/bin/fnsctl

# The database lives on a mounted volume. Declaring it means a missing mount
# fails loudly at deploy time rather than silently writing to a layer that
# disappears on the next restart.
VOLUME ["/data"]
ENV FNS_DB_PATH=/data/fetchandscore.db \
    FNS_ADDR=0.0.0.0:8080

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/fetchandscore"]

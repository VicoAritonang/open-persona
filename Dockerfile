# The OpenPersona server (HTTP API, gRPC and MCP on one port), for projects
# that are not written in Go.
#
#   docker build --build-arg VERSION=$(git describe --tags --always) -t openpersona .
#   docker run -p 8080:8080 --env-file .env -v ./data:/data -v ./packs:/packs openpersona

FROM golang:1.26 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /openpersona ./cmd/openpersona

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="openpersona" \
      org.opencontainers.image.description="Persona and context engineering for AI products: HTTP, gRPC and MCP server" \
      org.opencontainers.image.source="https://github.com/VicoAritonang/open-persona" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /openpersona /openpersona
VOLUME ["/data", "/packs"]
EXPOSE 8080
ENTRYPOINT ["/openpersona"]
CMD ["serve", "-addr", ":8080", "-db", "/data/memory.db", "-personas", "/packs"]

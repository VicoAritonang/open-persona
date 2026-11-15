# The HTTP server, for projects that are not written in Go.
#
#   docker build -t openpersona .
#   docker run -p 8080:8080 --env-file .env -v ./data:/data -v ./packs:/packs openpersona

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /openpersona ./cmd/openpersona

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /openpersona /openpersona
VOLUME ["/data", "/packs"]
EXPOSE 8080
ENTRYPOINT ["/openpersona"]
CMD ["serve", "-addr", ":8080", "-db", "/data/memory.db", "-personas", "/packs"]

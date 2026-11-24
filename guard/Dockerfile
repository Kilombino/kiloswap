# kiloswap-guard: a static binary on distroless, as nonroot.
FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /kiloswap-guard ./cmd/kiloswap-guard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /kiloswap-guard /kiloswap-guard
USER nonroot:nonroot
ENTRYPOINT ["/kiloswap-guard"]

# Static CGO-free control-plane image.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /defect-drainer ./cmd/defect-drainer

FROM scratch
COPY --from=build /defect-drainer /defect-drainer
ENV HOST=0.0.0.0
EXPOSE 8788
HEALTHCHECK --interval=10s --timeout=3s --retries=5 CMD ["/defect-drainer", "healthcheck"]
ENTRYPOINT ["/defect-drainer"]
CMD ["serve"]

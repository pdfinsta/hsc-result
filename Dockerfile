FROM golang:1.22 AS build
WORKDIR /src
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -o /app .

FROM chromedp/headless-shell:latest
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /app /app
ENV NO_SANDBOX=1 TRUST_PROXY=1 PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app"]

FROM golang:1.24.1-bookworm

WORKDIR /usr/src/app
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
RUN GOOS=linux go build -o /entrypoint .

ENTRYPOINT [ "/entrypoint" ]
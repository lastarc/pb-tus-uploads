FROM golang:alpine AS server-builder

RUN apk add --no-cache git

RUN mkdir -p /tmp/pb-tus-uploader
WORKDIR /tmp/pb-tus-uploader

COPY go.mod go.sum main.go ./

RUN go build -o server .

FROM node:alpine AS ui-builder

RUN corepack enable

WORKDIR /tmp

COPY ui/package.json ui/pnpm-lock.yaml ./

RUN pnpm i

COPY ./ui/ ./

RUN pnpm build

FROM alpine:latest

RUN mkdir -p /pb
WORKDIR /pb

COPY --from=server-builder /tmp/pb-tus-uploader/server ./server
COPY --from=ui-builder /tmp/dist ./pb_public

EXPOSE 8080

CMD ["./server", "serve", "--http=0.0.0.0:8080"]

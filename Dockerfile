FROM alpine:latest as builder

RUN apk add --no-cache git

RUN git clone https://github.com/lastarc/pb-tus-uploader.git /tmp/pb-tus-uploader
RUN cd /tmp/pb-tus-uploader && go build -o server .

FROM alpine:latest

RUN mkdir -p /pb/
COPY --from=builder /tmp/pb-tus-uploader/server /pb/server

EXPOSE 8080

# start PocketBase
CMD ["/pb/pocketbase", "serve", "--http=0.0.0.0:8080"]

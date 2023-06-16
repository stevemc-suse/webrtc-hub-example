FROM registry.suse.com/bci/golang:1.20 AS build

WORKDIR /usr/src/app

# pre-copy/cache go.mod for pre-downloading dependencies and only redownloading them in subsequent builds if they change
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
RUN go build -v -o ./webrtc-hub ./...

FROM registry.suse.com/bci/bci-busybox:15.5 AS run

COPY --from=build /usr/src/app/webrtc-hub .
COPY ./index.html .

CMD [ "./webrtc-hub" ]

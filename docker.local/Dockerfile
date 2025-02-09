FROM golang:1.14.9-alpine3.12 as blobber_build

RUN apk add --update --no-cache build-base linux-headers git cmake bash perl grep

# Install Herumi's cryptography
RUN apk add gmp gmp-dev openssl-dev && \
    cd /tmp && \
    wget -O - https://github.com/herumi/mcl/archive/master.tar.gz | tar xz && \
    mv mcl* mcl && \
    make -C mcl -j $(nproc) lib/libmclbn256.so install && \
    cp mcl/lib/libmclbn256.so /usr/local/lib && \
    rm -R /tmp/mcl
#TODO: create shared image and remove code duplicates!
RUN git clone https://github.com/herumi/bls /tmp/bls && \
    cd /tmp/bls && \
    git submodule init && \
    git submodule update && \
    make -j $(nproc) install && \
    cd - && \
    rm -R /tmp/bls

ENV SRC_DIR=/blobber
ENV GO111MODULE=on

# Download the dependencies:
# Will be cached if we don't change mod/sum files
COPY ./go.mod          ./go.sum          $SRC_DIR/
RUN cd $SRC_DIR && go mod download

#Add the source code
ADD ./code/go/0chain.net $SRC_DIR/code/go/0chain.net

WORKDIR $SRC_DIR/code/go/0chain.net/blobber

ARG GIT_COMMIT
ENV GIT_COMMIT=$GIT_COMMIT
RUN go build -v -tags "bn256 development" -gcflags "all=-N -l" -ldflags "-X 0chain.net/core/build.BuildTag=$GIT_COMMIT"

# Copy the build artifact into a minimal runtime image:
FROM golang:1.14.9-alpine3.12
RUN apk add gmp gmp-dev openssl-dev git
COPY --from=blobber_build  /usr/local/lib/libmcl*.so \
                        /usr/local/lib/libbls*.so \
                        /usr/local/lib/

RUN git clone --branch v1.4.1 https://github.com/go-delve/delve
WORKDIR ./delve
RUN go install ./cmd/dlv

ENV APP_DIR=/blobber
WORKDIR $APP_DIR
COPY --from=blobber_build $APP_DIR/code/go/0chain.net/blobber/blobber $APP_DIR/bin/blobber

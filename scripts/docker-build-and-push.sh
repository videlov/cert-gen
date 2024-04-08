#!/bin/bash

IMG=videlov/cert-gen:0.0.3

docker build -t ${IMG} .
docker login
docker push ${IMG}
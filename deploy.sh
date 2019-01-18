#!/usr/bin/env bash

GIT_SHA=$(git rev-parse --short=9 HEAD)
IMAGE_NAME="containers.schibsted.io/finntech/redisbench:${GIT_SHA}"

echo "Building docker image ${IMAGE_NAME}"
docker build -t "${IMAGE_NAME}" . || exit 2

echo "Pushing docker image ${IMAGE_NAME}"
docker push "${IMAGE_NAME}" || exit 3

echo "UPLOADED DOCKER IMAGE ${IMAGE_NAME}"

echo "Deploying fiaas"

mvn deploy:deploy-file -DgroupId=no.finntech \
    -DartifactId=redisbench \
    -Dclassifier=fiaas \
    -Dfile=./fiaas.yml \
    -Dpackaging=yml \
    -Dversion="${GIT_SHA}-SNAPSHOT" \
    -Durl=https://mavenproxy.finntech.no/finntech-internal-snapshot || exit 4

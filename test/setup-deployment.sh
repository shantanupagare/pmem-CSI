#!/bin/bash

set -o errexit
set -o pipefail

TEST_DIRECTORY=${TEST_DIRECTORY:-$(dirname $(readlink -f $0))}
source ${TEST_CONFIG:-${TEST_DIRECTORY}/test-config.sh}

CLUSTER=${CLUSTER:-clear-govm}
REPO_DIRECTORY="${REPO_DIRECTORY:-$(dirname $(dirname $(readlink -f $0)))}"
WORK_DIRECTORY="${WORK_DIRECTORY:-${REPO_DIRECTORY}/_work/${CLUSTER}}"
KUBECTL="${KUBECTL:-${WORK_DIRECTORY}/ssh-${CLUSTER} kubectl}"
KUBERNETES_VERSION="$(${KUBECTL} version --short | grep 'Server Version' | \
	sed -e 's/.*: v\([0-9]*\)\.\([0-9]*\)\..*/\1.\2/')"
TEST_DEVICEMODE=${TEST_DEVICEMODE:-lvm}
DEPLOYMENT_DIRECTORY="${REPO_DIRECTORY}/deploy/kubernetes-$KUBERNETES_VERSION"
TEST_INSECURE_REGISTRIES=${TEST_INSECURE_REGISTRIES:-$(awk '{NF=NF-1;gsub(".*@","");print $NF}' ${WORK_DIRECTORY}/ssh-${CLUSTER}):5000}
TEMPLATE_IP=${TEMPLATE_IP:-$(grep "image:.*5000" ${DEPLOYMENT_DIRECTORY}/*  | awk '{gsub (":5000.*",":5000");print $NF; exit}')}
DEPLOYMENT_FILES=(
        pmem-csi-${TEST_DEVICEMODE}-testing.yaml
        pmem-storageclass-ext4.yaml
        pmem-storageclass-xfs.yaml
        pmem-storageclass-cache.yaml
)

echo "$KUBERNETES_VERSION" > $WORK_DIRECTORY/kubernetes.version
for deployment_file in ${DEPLOYMENT_FILES[@]}; do
	sed "s|${TEMPLATE_IP}|${TEST_INSECURE_REGISTRIES// */}|g" ${DEPLOYMENT_DIRECTORY}/${deployment_file} | ${KUBECTL} apply -f -
done

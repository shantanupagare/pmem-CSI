#!/bin/bash
#
# Implements the first-boot configuration of the different virtual machines
# for Fedora running in GoVM.
#
# This script runs *inside* the cluster. All setting env variables
# used by it must be passed in explicitly via ssh and it must run as root.

set -x
set -o errexit # TODO: replace with explicit error checking and error messages.
set -o pipefail

: ${INIT_KUBERNETES:=true}
HOSTNAME=${HOSTNAME:-$1}
IPADDR=${IPADDR:-127.0.0.1}

function error_handler(){
    local line="${1}"
    echo >&2 "ERROR: the command '${BASH_COMMAND}' at $0:${line} failed"
}
trap 'error_handler ${LINENO}' ERR


# For PMEM.
packages+=" ndctl"

# Some additional utilities.
packages+=" device-mapper-persistent-data lvm2"

if ${INIT_KUBERNETES}; then
    case $TEST_CRI in
        docker|containerd)
            cat <<'EOF' > /etc/yum.repos.d/docker-ce.repo
[docker-ce-stable]
name=Docker CE Stable - $basearch
baseurl=https://download.docker.com/linux/centos/7/$basearch/stable
enabled=1
gpgcheck=1
gpgkey=https://download.docker.com/linux/centos/gpg
EOF
            if [ $TEST_CRI = docker ]; then
                packages+=" docker-ce-3:19.03.2-3.el7"
                cri_daemon=docker
            else
                packages+=" containerd.io-0:1.2.10-3.2.el7"
                cri_daemon=containerd
            fi
            ;;
        crio)
            # https://kubernetes.io/docs/setup/production-environment/container-runtimes/#cri-o
            #
            # In theory, the version of CRI-O should match the
            # corresponding Kubernetes release.  In practice, CentOS
            # currently only provides 1.15 (see
            # https://github.com/cri-o/cri-o/issues/1141 for reason
            # why we rely on CentOS) as the latest version and that
            # happens to work with all Kubernetes versions that we
            # currently test with.
            cat <<'EOF' >/etc/yum.repos.d/crio.repo
[crio]
name=CRI-O release repo
baseurl=https://cbs.centos.org/repos/paas7-crio-115-release/x86_64/os/
enabled=1
gpgcheck=0
EOF
            packages+=" cri-o"
            cri_daemon=cri-o
            ;;
        *)
            echo "ERROR: unsupported TEST_CRI=$TEST_CRI"
	    exit 1
            ;;
    esac

    # Common to CRI-O and containerd, probably okay for Docker (https://kubernetes.io/docs/setup/production-environment/container-runtimes/)
    mkdir -p /etc/modules-load.d/
    cat > /etc/modules-load.d/kubernetes.conf <<EOF
overlay
br_netfilter
EOF

    modprobe overlay
    modprobe br_netfilter

    # Setup required sysctl params, these persist across reboots.
    cat > /etc/sysctl.d/99-kubernetes-cri.conf <<EOF
net.bridge.bridge-nf-call-iptables  = 1
net.ipv4.ip_forward                 = 1
net.bridge.bridge-nf-call-ip6tables = 1
EOF
    sysctl --system

    # Install according to https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/install-kubeadm/
    setenforce 0
    sed -i --follow-symlinks 's/SELINUX=enforcing/SELINUX=disabled/g' /etc/sysconfig/selinux

    cat <<EOF > /etc/yum.repos.d/kubernetes.repo
[kubernetes]
name=Kubernetes
baseurl=https://packages.cloud.google.com/yum/repos/kubernetes-el7-x86_64
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://packages.cloud.google.com/yum/doc/yum-key.gpg https://packages.cloud.google.com/yum/doc/rpm-package-key.gpg
EOF

    # For the sake of reproducibility, use fixed versions.
    # List generated with:
    # for v in 1.13 1.14 1.15 1.16; do for i in kubelet kubeadm kubectl; do echo "$i-$(sudo yum --showduplicates list kubelet | grep " $v"  | sed -e 's/.* \([0-9]*\.[0-9]*\.[0-9]*[^ ]*\).*/\1/' | sort -u  | tail -n 1)"; done; done
    case ${TEST_KUBERNETES_VERSION} in
        1.13) packages+=" kubelet-1.13.9-0 kubeadm-1.13.9-0 kubectl-1.13.9-0";;
        1.14) packages+=" kubelet-1.14.7-0 kubeadm-1.14.7-0 kubectl-1.14.7-0";;
        1.15) packages+=" kubelet-1.15.4-0 kubeadm-1.15.4-0 kubectl-1.15.4-0";;
        1.16) packages+=" kubelet-1.16.0-0 kubeadm-1.16.0-0 kubectl-1.16.0-0";;
        *) echo >&2 "Kubernetes version ${TEST_KUBERNETES_VERSION} not supported, package list in $0 must be updated."; exit 1;;
    esac
    packages+=" --disableexcludes=kubernetes"
fi

# Sometimes we hit a bad mirror and get "Failed to synchronize cache for repo ...".
# https://unix.stackexchange.com/questions/487635/fedora-29-failed-to-synchronize-cache-for-repo-fedora-modular
# suggests to try again after a `dnf update --refresh`, so that's what we do here for
# a maximum of 5 attempts.
cnt=0
while ! yum install -y $packages; do
    if [ $cnt -ge 5 ]; then
        echo "yum install failed repeatedly, giving up"
        exit 1
    fi
    cnt=$(($cnt + 1))
    # If it works, proceed immediately. If it fails, sleep and try again without aborting on an error.
    if ! dnf update --refresh; then
        sleep 20
        dnf update --refresh || true
    fi
done

if $INIT_KUBERNETES; then
    # Upstream kubelet looks in /opt/cni/bin, actual files are in
    # /usr/libexec/cni from
    # containernetworking-plugins-0.8.1-1.fc30.x86_64.
    mkdir -p /opt/cni
    ln -s /usr/libexec/cni /opt/cni/bin

    # Testing may involve a Docker registry running on the build host (see
    # TEST_LOCAL_REGISTRY and TEST_PMEM_REGISTRY). We need to trust that
    # registry, otherwise Docker will fail to pull images from it.
    mkdir -p /etc/docker
    cat >/etc/docker/daemon.json <<EOF
{ "insecure-registries": [ $(echo $INSECURE_REGISTRIES | sed 's|^|"|g;s| |", "|g;s|$|"|') ] }
EOF
    mkdir -p /etc/containers
    cat >/etc/containers/registries.conf <<EOF
[registries.insecure]
registries = [ $(echo $INSECURE_REGISTRIES | sed 's|^|"|g;s| |", "|g;s|$|"|') ]

# We need to configure docker.io as default registry (https://github.com/kubernetes/minikube/issues/2835).
[registries.search]
registries = ['docker.io']
EOF
    mkdir -p /etc/containerd
    cat >/etc/containerd/config.toml <<EOF
[plugins.cri.registry.mirrors]
EOF
    for registry in $INSECURE_REGISTRIES; do
        cat >>/etc/containerd/config.toml <<EOF
  [plugins.cri.registry.mirrors."$registry"]
    endpoint = ["http://$registry"]
EOF
    done

    # Proxy settings for the different container runtimes are injected into
    # their environment.
    for cri in crio docker containerd; do
        mkdir -p /etc/systemd/system/$cri.service.d
        cat >/etc/systemd/system/$cri.service.d/proxy.conf <<EOF
[Service]
Environment="HTTP_PROXY=${HTTP_PROXY}" "HTTPS_PROXY=${HTTPS_PROXY}" "NO_PROXY=${NO_PROXY}"
EOF
    done

    # kubelet must start after the container runtime that it depends on.
    mkdir -p /etc/systemd/system/kubelet.service.d/
    cat >/etc/systemd/system/kubelet.service.d/10-cri.conf <<EOF
[Unit]
After=$cri_daemon.service
EOF

    # Workaround for crio 1.15.1 (https://github.com/kubernetes/minikube/issues/5323).
    if [ $TEST_CRI = "crio" ] && rpm -q cri-o | grep -q -e -1.15.1-; then
        mkdir -p /etc/systemd/system/kubelet.service.d/
        cat >/etc/systemd/system/kubelet.service.d/10-crio-1151.conf <<EOF
[Unit]
Environment="GRPC_GO_REQUIRE_HANDSHAKE=off"
EOF
    fi

    # Configure Docker as suggested in https://kubernetes.io/docs/setup/production-environment/container-runtimes/#docker,
    # see also https://github.com/kubernetes/kubeadm/issues/1394.
    cat >>/etc/docker/daemon.json <<EOF
{
  "exec-opts": ["native.cgroupdriver=systemd"],
  "log-driver": "json-file",
  "log-opts": {
    "max-size": "100m"
  },
  "storage-driver": "overlay2"
}
EOF

    # And also containerd.
    mkdir -p /etc/containerd
    cat >>/etc/containerd/config.toml <<EOF
[plugins.cri]
  systemd_cgroup = true
EOF

    # https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/install-kubeadm/#configure-cgroup-driver-used-by-kubelet-on-control-plane-node
    sed -i -e 's/KUBELET_EXTRA_ARGS=/KUBELET_EXTRA_ARGS=--cgroup-driver=systemd /' /etc/sysconfig/kubelet

    update-alternatives --set iptables /usr/sbin/iptables-legacy
    systemctl daemon-reload
    # Stop them, just in case.
    systemctl stop $cri_daemon kubelet
    # kubelet will be started by kubeadm after configuring it.
    systemctl enable $cri_daemon kubelet
    systemctl start $cri_daemon
fi

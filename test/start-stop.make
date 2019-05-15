# Brings up the emulator environment:
# - starts a Kubernetes cluster with NVDIMMs as described in https://github.com/qemu/qemu/blob/bd54b11062c4baa7d2e4efadcf71b8cfd55311fd/docs/nvdimm.txt
# - generate pmem secrets if necessary
start: test/setup-ca-kubernetes.sh _work/.setupcfssl-stamp
	. test/test-config.sh && \
	test/start-kubernetes.sh && \
	if ! [ -e _work/$(CLUSTER)/secretsdone ] || [ $$(_work/$(CLUSTER)/ssh-$(CLUSTER) kubectl get secrets | grep -e pmem-csi-node-secrets -e pmem-csi-registry-secrets | wc -l) -ne 2 ]; then \
		KUBECTL="$(PWD)/_work/$(CLUSTER)/ssh-$(CLUSTER) kubectl" PATH='$(PWD)/_work/bin/:$(PATH)' ./test/setup-ca-kubernetes.sh && \
		touch _work/$(CLUSTER)/secretsdone; \
	fi \
	&& test/setup-deployment.sh
	@ echo "The test cluster is ready. Log in with _work/ssh-$(CLUSTER), run kubectl once logged in."
	@ echo "Alternatively, KUBECONFIG=$$(pwd)/_work/kube.config can also be used directly."
	@ echo "To try out the pmem-csi driver persistent volumes:"
	@ echo "   cat deploy/kubernetes-$$(cat _work/$(CLUSTER)/kubernetes.version)/pmem-pvc.yaml | _work/ssh-$(CLUSTER) kubectl create -f -"
	@ echo "   cat deploy/kubernetes-$$(cat _work/$(CLUSTER)/kubernetes.version)/pmem-app.yaml | _work/ssh-$(CLUSTER) kubectl create -f -"
	@ echo "To try out the pmem-csi driver cache volumes:"
	@ echo "   cat deploy/kubernetes-$$(cat _work/$(CLUSTER)/kubernetes.version)/pmem-pvc-cache.yaml | _work/ssh-$(CLUSTER) kubectl create -f -"
	@ echo "   cat deploy/kubernetes-$$(cat _work/$(CLUSTER)/kubernetes.version)/pmem-app-cache.yaml | _work/ssh-$(CLUSTER) kubectl create -f -"

stop:
	. test/test-config.sh && \
	_work/$(CLUSTER)/stop.sh

restart:
	. test/test-config.sh && \
	_work/$(CLUSTER)/restart.sh

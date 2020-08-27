/*
Copyright 2019 Intel Corporation.

SPDX-License-Identifier: Apache-2.0
*/

package tls

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	"github.com/intel/pmem-csi/test/e2e/deploy"
	pmempod "github.com/intel/pmem-csi/test/e2e/pod"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
)

var _ = deploy.DescribeForAll("TLS", func(d *deploy.Deployment) {
	f := framework.NewDefaultFramework("tls")

	// All of the following pod names, namespaces and ports match
	// those in the current deployment files.

	var nodePod *v1.Pod
	BeforeEach(func() {
		// Find one node driver pod.
		label := labels.SelectorFromSet(labels.Set(map[string]string{"app": "pmem-csi-node"}))
		pods, err := f.ClientSet.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{LabelSelector: label.String()})
		framework.ExpectNoError(err, "list PMEM-CSI node pods")
		Expect(pods.Items).NotTo(BeEmpty(), "have PMEM-CSI node pods")
		nodePod = &pods.Items[0]
	})

	Context("controller", func() {
		It("is secure", func() {
			checkTLS(f, "pmem-csi-controller-0.pmem-csi-controller.default")
		})
	})
	Context("node", func() {
		It("is secure", func() {
			checkTLS(f, nodePod.Status.PodIP)
		})
	})
})

func checkTLS(f *framework.Framework, server string) {
	containerName := "nmap"
	root := int64(0)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      containerName,
			Namespace: f.Namespace.Name,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "nmap",
					Image:   os.Getenv("PMEM_CSI_IMAGE"),
					Command: []string{"sleep", "1000000"},
				},
			},
			// Needs to have root privileges to run nmap
			SecurityContext: &v1.PodSecurityContext{
				RunAsUser:  &root,
				RunAsGroup: &root,
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	By(fmt.Sprintf("Creating pod %s", pod.Name))
	ns := f.Namespace.Name
	podClient := f.PodClientNS(ns)
	createdPod := podClient.Create(pod)
	defer func() {
		By("delete the pod")
		podClient.DeleteSync(createdPod.Name, metav1.DeleteOptions{}, framework.DefaultPodDeletionTimeout)
	}()
	podErr := e2epod.WaitForPodRunningInNamespace(f.ClientSet, createdPod)
	framework.ExpectNoError(podErr, "running pod")

	// Install and patch nmap.
	pmempod.RunInPod(f, os.Getenv("REPO_ROOT")+"/test/e2e/tls", []string{"nmap-ssl-enum-ciphers.patch"},
		strings.Join([]string{
			fmt.Sprintf("https_proxy=%s swupd bundle-add nmap patch >&2", os.Getenv("HTTPS_PROXY")),
			"patch /usr/share/nmap/scripts/ssl-enum-ciphers.nse <nmap-ssl-enum-ciphers.patch",
		},
			" && "),
		ns, pod.Name, containerName)

	Eventually(func() int {
		By("scanning ports")
		// We have to patch nmap because of https://github.com/nmap/nmap/issues/1187#issuecomment-587031079.
		output, _ := pmempod.RunInPod(f, os.Getenv("REPO_ROOT")+"/test/e2e/tls", []string{"nmap-ssl-enum-ciphers.patch"},
			fmt.Sprintf("nmap --script +ssl-enum-ciphers --open -Pn %s 2>&1", server),
			ns, pod.Name, containerName)

		// Now analyze all ports and the ciphers found for them.
		// The output will be something like this:
		//   Nmap scan report for pmem-csi-controller-0.pmem-csi-controller.default (10.44.0.1)
		//   Host is up (0.00013s latency).
		//   rDNS record for 10.44.0.1: pmem-csi-controller-0.pmem-csi-controller.default.svc.cluster.local
		//   Not shown: 997 closed ports
		//   PORT      STATE SERVICE
		//   8000/tcp  open  http-alt
		//   | ssl-enum-ciphers:
		//   |   TLSv1.0:
		//   |     ciphers:
		//   |       TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA (ecdh_x25519) - A
		//   |       TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA (ecdh_x25519) - A
		//   |     compressors:
		//   |       NULL
		//   |     cipher preference: server
		//   |   TLSv1.1:
		//   |     ciphers:
		//   |       TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA (ecdh_x25519) - A
		//   |       TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA (ecdh_x25519) - A
		//   |     compressors:
		//   |       NULL
		//   |     cipher preference: server
		//   |   TLSv1.2:
		//   |     ciphers:
		//   |       TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 (ecdh_x25519) - A
		//   |       TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384 (ecdh_x25519) - A
		//   |       TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256 (ecdh_x25519) - A
		//   |       TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA (ecdh_x25519) - A
		//   |       TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA (ecdh_x25519) - A
		//   |     compressors:
		//   |       NULL
		//   |     cipher preference: server
		//   |_  least strength: A
		//   10000/tcp open  snet-sensor-mgmt
		//   | ssl-enum-ciphers:
		//   |   TLSv1.2:
		//   |     ciphers:
		//   |       TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256 (ecdh_x25519) - A
		//   |       TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 (ecdh_x25519) - A
		//   |       TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384 (ecdh_x25519) - A
		//   |       TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256 (ecdh_x25519) - A
		//   |       TLS_ECDHE_ECDSA_WITH_RC4_128_SHA (ecdh_x25519) - C
		//   |     compressors:
		//   |       NULL
		//   |     cipher preference: client
		//   |     warnings:
		//   |       Broken cipher RC4 is deprecated by RFC 7465
		//   |_  least strength: C
		//   10002/tcp open  documentum
		//   MAC Address: D2:82:BC:59:C9:CC (Unknown)
		//
		//   Nmap done: 1 IP address (1 host up) scanned in 0.34 seconds

		// We need the full strings if the comparison below fails.
		old := format.TruncatedDiff
		defer func() {
			format.TruncatedDiff = old
		}()
		format.TruncatedDiff = false

		re := regexp.MustCompile(`(?m)^([[:digit:]]+)/.* open .*\n((?:^\|.*\n)*)`)
		ports := re.FindAllStringSubmatch(output, -1)
		for _, entry := range ports {
			port, ciphers := entry[1], entry[2]
			switch port {
			case "10002", "10010", "10011":
				// The socat debugging port and metrics ports. Can be ignored.
				continue
			}
			// All other ports must use TLS, with exactly the
			// ciphers that we want enabled. All of them should be rated A.
			//
			// The exact output depends on:
			// - the version of nmap (locked on release branches by fixing the Clear Linux
			//   release, varies on development branches)
			// - the version of Go that is being used for building PMEM-CSI (locked
			//   in our Dockerfile)
			// - the generated keys and thus the deployment method (the
			//   current list is for "make start" and keys created with
			//   test/setup-ca.sh, which in turn uses cfssl as installed
			//   by test/test.make, at least in the CI)
			//
			// This list may have to be adapted when changing either of these.
			Expect(ciphers).To(Equal(`| ssl-enum-ciphers: 
|   TLSv1.2: 
|     ciphers: 
|       TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256 (ecdh_x25519) - A
|       TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 (ecdh_x25519) - A
|       TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384 (ecdh_x25519) - A
|       TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256 (ecdh_x25519) - A
|     compressors: 
|       NULL
|     cipher preference: client
|_  least strength: A
`), "ciphers for port %s in %s", port, server)
		}

		return len(ports)
	}, "1m", "5s").Should(BeNumerically(">", 0), "no open ports found, networking down?")
}

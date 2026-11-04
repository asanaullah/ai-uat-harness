// Assisted by Claude Opus
package test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Failure modes: an over-requesting pod waits in Pending with a legible
// reason instead of being silently queued, and deleting it completes.

var (
	hogs       []string
	namespace  string
	readerFile string
)

// The Describe body registers one It per hog at tree construction, before
// BeforeSuite runs, so the list comes from the environment here.
func init() {
	for _, name := range strings.Split(os.Getenv("HOGS"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			hogs = append(hogs, name)
		}
	}
	if len(hogs) == 0 {
		fmt.Fprintln(os.Stderr, "HOGS must name at least one pod")
		os.Exit(1)
	}
}

func TestNegative(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Failure Modes Suite")
}

var _ = BeforeSuite(func() {
	content, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	Expect(err).NotTo(HaveOccurred(), "reading the pod's namespace")
	namespace = strings.TrimSpace(string(content))
	readerFile = os.Getenv("READER_TOKEN_FILE")
	Expect(readerFile).NotTo(BeEmpty(), "READER_TOKEN_FILE must be set")
})

// The identity the playbooks run under; it may get and delete pods.
func readerClient() *kubernetes.Clientset {
	cfg, err := rest.InClusterConfig()
	Expect(err).NotTo(HaveOccurred(), "reading the pod's own service account")
	token, err := os.ReadFile(readerFile)
	Expect(err).NotTo(HaveOccurred(), "reading the reader token")
	cfg.BearerToken = strings.TrimSpace(string(token))
	cfg.BearerTokenFile = ""
	clientset, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	return clientset
}

func unschedulableReason(pod *corev1.Pod) (string, string) {
	for i := range pod.Status.Conditions {
		cond := &pod.Status.Conditions[i]
		if cond.Type == corev1.PodScheduled {
			return string(cond.Status), cond.Reason
		}
	}
	return "", ""
}

var _ = Describe("Failure Modes", Label("pass-fail"), func() {
	var client *kubernetes.Clientset

	BeforeEach(func() {
		client = readerClient()
	})

	for _, hog := range hogs {
		It(fmt.Sprintf("%s should wait in Pending with a legible reason", hog), func() {
			Eventually(func(g Gomega) {
				pod, err := client.CoreV1().Pods(namespace).Get(
					context.TODO(), hog, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "reading %s", hog)
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodPending),
					"%s left Pending: it was queued or failed outright", hog)
				status, reason := unschedulableReason(pod)
				g.Expect(status).To(Equal("False"), "%s has no PodScheduled condition yet", hog)
				g.Expect(reason).To(Equal("Unschedulable"),
					"the scheduler did not say why %s cannot run", hog)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It(fmt.Sprintf("%s should delete cleanly", hog), func() {
			err := client.CoreV1().Pods(namespace).Delete(
				context.TODO(), hog, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred(), "asking to delete %s", hog)
			Eventually(func(g Gomega) {
				_, err := client.CoreV1().Pods(namespace).Get(
					context.TODO(), hog, metav1.GetOptions{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
					"%s is still there after its deletion was accepted", hog)
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	}
})

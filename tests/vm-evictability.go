// Assisted by Claude Opus
package test

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// A plain container pod yields to the Eviction API; a VM's launcher pod
// does not. This test establishes that asymmetry by asking the Eviction
// API directly, touching only pods it created itself.

var (
	vmURL     string
	vmName    string
	plainPod  string
	runID     string
	namespace string
	readerCfg *rest.Config
)

func TestVMEvictability(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VM Evictability Suite")
}

var _ = BeforeSuite(func() {
	vmURL = os.Getenv("VM_URL")
	vmName = os.Getenv("VM_NAME")
	plainPod = os.Getenv("PLAIN_POD")
	runID = os.Getenv("RUN_ID")
	Expect(vmURL).NotTo(BeEmpty(), "VM_URL must be set")
	Expect(vmName).NotTo(BeEmpty(), "VM_NAME must be set")
	Expect(plainPod).NotTo(BeEmpty(), "PLAIN_POD must be set")
	Expect(runID).NotTo(BeEmpty(), "RUN_ID must be set")

	raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	Expect(err).NotTo(HaveOccurred(), "reading the pod's namespace")
	namespace = strings.TrimSpace(string(raw))

	cfg, err := rest.InClusterConfig()
	Expect(err).NotTo(HaveOccurred(), "reading the pod's own service account")
	token, err := os.ReadFile(os.Getenv("READER_TOKEN_FILE"))
	Expect(err).NotTo(HaveOccurred(), "reading the reader token")
	cfg.BearerToken = strings.TrimSpace(string(token))
	cfg.BearerTokenFile = ""
	readerCfg = cfg
})

func readerClientset() *kubernetes.Clientset {
	clientset, err := kubernetes.NewForConfig(readerCfg)
	Expect(err).NotTo(HaveOccurred())
	return clientset
}

// The identity the playbooks run under; it may create pod evictions.
func evict(podName string) error {
	cfg := rest.CopyConfig(readerCfg)
	cfg.GroupVersion = &schema.GroupVersion{Group: "", Version: "v1"}
	cfg.APIPath = "/api"
	cfg.NegotiatedSerializer = serializer.NewCodecFactory(kscheme.Scheme).WithoutConversion()
	client, err := rest.RESTClientFor(cfg)
	if err != nil {
		return err
	}
	return client.Post().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("eviction").
		Body(&policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace}}).
		Do(context.TODO()).
		Error()
}

func fetch(path string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(vmURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, io.ErrUnexpectedEOF
	}
	return body, nil
}

var _ = Describe("VM Evictability", Label("pass-fail"), func() {
	var vmPodName, beforeBoot string

	It("should boot the guest from the shared volume", func() {
		Eventually(func(g Gomega) {
			body, err := fetch("/marker")
			g.Expect(err).NotTo(HaveOccurred(), "guest not serving yet")
			g.Expect(string(body)).To(Equal(runID + " from vm-evictability\n"),
				"the marker did not come from this run's guest: %s", string(body))
		}, 10*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("should record the guest and both pods' eviction metadata", func() {
		body, err := fetch("/boot-id")
		Expect(err).NotTo(HaveOccurred())
		beforeBoot = strings.TrimSpace(string(body))
		Expect(beforeBoot).NotTo(BeEmpty())

		client := readerClientset()
		pods, err := client.CoreV1().Pods(namespace).List(context.TODO(),
			metav1.ListOptions{LabelSelector: "vm.kubevirt.io/name=" + vmName})
		Expect(err).NotTo(HaveOccurred(), "listing the VM's launcher pods")
		Expect(pods.Items).NotTo(BeEmpty(), "no virt-launcher pod found for the VM")
		vmPodName = pods.Items[0].Name

		plain, err := client.CoreV1().Pods(namespace).Get(context.TODO(), plainPod, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "reading the plain pod")
		AddReportEntry("vm_pod_qos", pods.Items[0].Status.QOSClass)
		AddReportEntry("plain_pod_qos", plain.Status.QOSClass)

		budgets, err := client.PolicyV1().PodDisruptionBudgets(namespace).List(context.TODO(), metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred(), "listing the disruption budgets")
		names := []string{}
		for _, pdb := range budgets.Items {
			names = append(names, pdb.Name)
		}
		// Newer KubeVirt guards VMs with a webhook instead of a budget; recorded
// so the mechanism shows up in the artifacts either way.
		// instead of a PodDisruptionBudget; the list is recorded so the
		// mechanism in use shows up in the run artifacts either way.
		AddReportEntry("disruption_budgets", names)
	})

	It("should evict the plain pod, and it should go away", func() {
		Expect(evict(plainPod)).NotTo(HaveOccurred(),
			"evicting the plain pod was not accepted; a container pod should yield")
		Eventually(func() bool {
			_, err := readerClientset().CoreV1().Pods(namespace).Get(context.TODO(), plainPod, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 3*time.Minute, 5*time.Second).Should(BeTrue(),
			"the plain pod is still there after being evicted")
	})

	It("should refuse to evict the VM's pod and keep the guest running", func() {
		err := evict(vmPodName)
		Expect(err).NotTo(BeNil(),
			"evicting the VM's launcher pod was accepted; the VM should not yield like a container")
		Expect(apierrors.IsNotFound(err)).To(BeFalse(),
			"the VM's launcher pod disappeared before the eviction was even attempted: %v", err)
		AddReportEntry("vm_eviction_refusal", err.Error())

		// The refusal may trigger a live migration to a new launcher pod;
		// the guest must stay up through it, on the same boot.
		Eventually(func(g Gomega) {
			body, err := fetch("/marker")
			g.Expect(err).NotTo(HaveOccurred(), "the guest stopped serving after the eviction attempt")
			g.Expect(string(body)).To(Equal(runID + " from vm-evictability\n"),
				"the marker changed after the eviction attempt: %s", string(body))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		body, err := fetch("/boot-id")
		Expect(err).NotTo(HaveOccurred(), "the guest stopped serving the boot id")
		Expect(strings.TrimSpace(string(body))).To(Equal(beforeBoot),
			"the boot id changed: the eviction disturbed the guest instead of being refused or migrated")
	})
})

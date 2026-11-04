// Assisted by Claude Opus
package test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The virt-launcher pod reserves the guest's memory plus an overhead of
// its own, measured here at three guest sizes. The asserted invariant is
// that the node's reservation covers the promise; what the guest itself
// reports is recorded only, since the firmware carve-out grows with the
// guest and no single floor fits every size.

var (
	vmSpecs   []vmSpec
	namespace string
	readerCfg *rest.Config
)

type vmSpec struct {
	name string
	size string
}

// The Describe body registers one It per VM at tree construction, before
// BeforeSuite runs, so the list comes from the environment here.
func init() {
	for _, pair := range strings.Split(os.Getenv("VM_SPECS"), ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			fmt.Fprintln(os.Stderr, "VM_SPECS entries must be name:size")
			os.Exit(1)
		}
		vmSpecs = append(vmSpecs, vmSpec{name: parts[0], size: parts[1]})
	}
	if len(vmSpecs) == 0 {
		fmt.Fprintln(os.Stderr, "VM_SPECS must name at least one VM")
		os.Exit(1)
	}
}

func TestVMMemoryOverhead(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VM Memory Overhead Suite")
}

var _ = BeforeSuite(func() {
	content, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	Expect(err).NotTo(HaveOccurred(), "reading the pod's namespace")
	namespace = strings.TrimSpace(string(content))

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

func launcherPod(vm string) (name, ip string) {
	Eventually(func(g Gomega) {
		pods, err := readerClientset().CoreV1().Pods(namespace).List(context.TODO(),
			metav1.ListOptions{LabelSelector: "vm.kubevirt.io/name=" + vm})
		g.Expect(err).NotTo(HaveOccurred(), "listing the launcher pods of %s", vm)
		g.Expect(pods.Items).NotTo(BeEmpty(), "no launcher pod for %s yet", vm)
		g.Expect(pods.Items[0].Status.PodIP).NotTo(BeEmpty(), "%s has no pod IP yet", vm)
		name = pods.Items[0].Name
		ip = pods.Items[0].Status.PodIP
	}, 5*time.Minute, 10*time.Second).Should(Succeed())
	return name, ip
}

func guestMemTotal(ip string) int64 {
	client := &http.Client{Timeout: 30 * time.Second}
	var body []byte
	Eventually(func(g Gomega) {
		resp, err := client.Get("http://" + ip + ":8080/meminfo")
		g.Expect(err).NotTo(HaveOccurred(), "reaching the guest at %s", ip)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(resp.StatusCode).To(Equal(200), "fetching the guest's meminfo")
		body = raw
	}, 5*time.Minute, 10*time.Second).Should(Succeed())

	fields := strings.Fields(strings.TrimSpace(string(body)))
	Expect(len(fields)).To(Equal(3), "unexpected meminfo line: %q", string(body))
	kb, err := strconv.ParseInt(fields[1], 10, 64)
	Expect(err).NotTo(HaveOccurred(), "parsing MemTotal: %q", string(body))
	return kb * 1024
}

var _ = Describe("VM Memory Overhead", Label("pass-fail"), func() {
	for _, spec := range vmSpecs {
		spec := spec
		It("should reserve at least the guest's memory for "+spec.size, func() {
			podName, _ := launcherPod(spec.name)
			pod, err := readerClientset().CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "reading the launcher pod")

			promised := resource.MustParse(spec.size)
			// The node accounts the sum over all containers, sidecars
			// included, not any single container's request.
			var requested, limited int64
			for _, container := range pod.Spec.Containers {
				if q, ok := container.Resources.Requests["memory"]; ok {
					requested += q.Value()
				}
				if q, ok := container.Resources.Limits["memory"]; ok {
					limited += q.Value()
				}
			}
			Expect(requested).To(BeNumerically(">", 0),
				"the launcher pod %s has no memory request", podName)
			Expect(limited).To(BeNumerically(">", 0),
				"the launcher pod %s has no memory limit", podName)
			Expect(requested).To(BeNumerically(">=", promised.Value()),
				"the launcher reserves %d bytes for a %s guest: less than the guest was promised",
				requested, spec.size)

			_, ip := launcherPod(spec.name)
			reported := guestMemTotal(ip)
			AddReportEntry("guest_"+spec.size+"_bytes", promised.Value())
			AddReportEntry("launcher_request_"+spec.size+"_bytes", requested)
			AddReportEntry("launcher_limit_"+spec.size+"_bytes", limited)
			AddReportEntry("guest_reported_"+spec.size+"_bytes", reported)
			AddReportEntry("overhead_"+spec.size+"_ratio", float64(requested)/float64(promised.Value()))
		})
	}
})

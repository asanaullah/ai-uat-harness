// Assisted by Claude Opus
package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// A secondary network is hand-sized and shared between VMs and containers,
// the realistic exhaustion case. This test fills one until the next claim
// is refused, then checks the refusal is legible, that a container draws
// from the same pool, that addresses come back on graceful delete, and
// that a force-deleted launcher pod does not shrink the pool. Observed
// through the Multus network-status annotation on pods and pod events.

var (
	testRange      string
	maxClaimants   int
	claimTimeout   time.Duration
	vmImage        string
	runID          string
	namespace      string
	readerCfg      *rest.Config
	nadName        string
	claimantPrefix string
	claimants      []string
	allClaimantVMs []string
)

func TestVMSecondaryIPAM(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VM Secondary-Network IPAM Suite")
}

var _ = BeforeSuite(func() {
	testRange = os.Getenv("TEST_RANGE")
	raw := os.Getenv("MAX_CLAIMANTS")
	var err error
	maxClaimants, err = strconv.Atoi(raw)
	Expect(err).NotTo(HaveOccurred(), "MAX_CLAIMANTS must be a number, got %q", raw)
	secs, err := strconv.Atoi(os.Getenv("CLAIM_TIMEOUT_SECONDS"))
	Expect(err).NotTo(HaveOccurred(), "CLAIM_TIMEOUT_SECONDS must be a number")
	claimTimeout = time.Duration(secs) * time.Second
	vmImage = os.Getenv("VM_IMAGE")
	runID = os.Getenv("RUN_ID")
	Expect(testRange).NotTo(BeEmpty(), "TEST_RANGE must be set")
	Expect(vmImage).NotTo(BeEmpty(), "VM_IMAGE must be set")
	Expect(runID).NotTo(BeEmpty(), "RUN_ID must be set")
	Expect(maxClaimants).To(BeNumerically(">", 0))

	raw2, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	Expect(err).NotTo(HaveOccurred(), "reading the pod's namespace")
	namespace = strings.TrimSpace(string(raw2))

	cfg, err := rest.InClusterConfig()
	Expect(err).NotTo(HaveOccurred(), "reading the pod's own service account")
	token, err := os.ReadFile(os.Getenv("READER_TOKEN_FILE"))
	Expect(err).NotTo(HaveOccurred(), "reading the reader token")
	cfg.BearerToken = strings.TrimSpace(string(token))
	cfg.BearerTokenFile = ""
	readerCfg = cfg

	// Names must be DNS safe; the run id is not.
	slug := regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(runID), "-")
	nadName = "ipam-" + slug + "-net"
	claimantPrefix = "ipam-" + slug
})

func readerClientset() *kubernetes.Clientset {
	clientset, err := kubernetes.NewForConfig(readerCfg)
	Expect(err).NotTo(HaveOccurred())
	return clientset
}

func dynamicClient() dynamic.Interface {
	client, err := dynamic.NewForConfig(readerCfg)
	Expect(err).NotTo(HaveOccurred())
	return client
}

var vmGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
var nadGVR = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions"}

// Leftovers from a crashed run are swept by the next run's preflight.
func claimLabels() map[string]interface{} {
	return map[string]interface{}{
		"app.kubernetes.io/managed-by": "uat-harness",
		"uat/run":                      runID,
		"ipam-test":                    nadName,
	}
}

func createNAD() {
	config := fmt.Sprintf(`{"cniVersion":"0.3.1","name":%q,"type":"ovn-k8s-cni-overlay",`+
		`"topology":"layer2","subnets":%q,"netAttachDefName":%q}`,
		nadName, testRange, namespace+"/"+nadName)
	nad := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.cni.cncf.io/v1",
		"kind":       "NetworkAttachmentDefinition",
		"metadata":   map[string]interface{}{"name": nadName, "labels": claimLabels()},
		"spec":       map[string]interface{}{"config": config},
	}}
	_, err := dynamicClient().Resource(nadGVR).Namespace(namespace).
		Create(context.TODO(), nad, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating the secondary network")
}

func deleteNAD() {
	_ = dynamicClient().Resource(nadGVR).Namespace(namespace).
		Delete(context.TODO(), nadName, metav1.DeleteOptions{})
}

func createClaimantVM(name string) {
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   map[string]interface{}{"name": name, "labels": claimLabels()},
		"spec": map[string]interface{}{
			"runStrategy": "Always",
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": map[string]interface{}{"ipam-test": nadName, "kubevirt.io/vm": name},
				},
				"spec": map[string]interface{}{
					"tolerations": []interface{}{map[string]interface{}{
						"key": "nvidia.com/gpu.product", "operator": "Exists", "effect": "NoSchedule",
					}},
					"terminationGracePeriodSeconds": 0,
					"domain": map[string]interface{}{
						"devices": map[string]interface{}{
							"disks": []interface{}{map[string]interface{}{
								"name": "containerdisk", "disk": map[string]interface{}{"bus": "virtio"},
							}},
							"interfaces": []interface{}{
								map[string]interface{}{"name": "default", "masquerade": map[string]interface{}{}},
								map[string]interface{}{"name": "secondary", "bridge": map[string]interface{}{}},
							},
							"rng": map[string]interface{}{},
						},
						"resources": map[string]interface{}{
							"requests": map[string]interface{}{"memory": "256Mi"},
						},
					},
					"networks": []interface{}{
						map[string]interface{}{"name": "default", "pod": map[string]interface{}{}},
						map[string]interface{}{"name": "secondary",
							"multus": map[string]interface{}{"networkName": nadName}},
					},
					"volumes": []interface{}{map[string]interface{}{
						"name": "containerdisk",
						"containerDisk": map[string]interface{}{"image": vmImage},
					}},
				},
			},
		},
	}}
	_, err := dynamicClient().Resource(vmGVR).Namespace(namespace).
		Create(context.TODO(), vm, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating claimant %s", name)
}

func deleteVM(name string) {
	_ = dynamicClient().Resource(vmGVR).Namespace(namespace).
		Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func launcherPod(vm string) string {
	pods, err := readerClientset().CoreV1().Pods(namespace).List(context.TODO(),
		metav1.ListOptions{LabelSelector: "vm.kubevirt.io/name=" + vm})
	Expect(err).NotTo(HaveOccurred(), "listing the launcher pods of %s", vm)
	if len(pods.Items) == 0 {
		return ""
	}
	return pods.Items[0].Name
}

// The tenant-visible record of what CNI allocated, written by Multus onto
// the pod; the VMI would need the guest agent, which CirrOS lacks.
func podSecondaryIP(podName string) string {
	if podName == "" {
		return ""
	}
	pod, err := readerClientset().CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	var entries []struct {
		Name string   `json:"name"`
		IPs  []string `json:"ips"`
	}
	if err := json.Unmarshal([]byte(pod.Annotations["k8s.v1.cni.cncf.io/network-status"]), &entries); err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Name == namespace+"/"+nadName && len(entry.IPs) > 0 {
			return strings.Split(entry.IPs[0], "/")[0]
		}
	}
	return ""
}

// The reason a claim failed lives in the pod's events, not its status: the
// pod sits in ContainerCreating while CNI retries forever.
func podFailureEvents(podName string) string {
	events, err := readerClientset().CoreV1().Events(namespace).List(context.TODO(),
		metav1.ListOptions{FieldSelector: "involvedObject.name=" + podName})
	if err != nil {
		return "(events unreadable: " + err.Error() + ")"
	}
	lines := []string{}
	for _, event := range events.Items {
		if event.Type == corev1.EventTypeNormal {
			continue
		}
		lines = append(lines, event.Reason+": "+event.Message)
	}
	if len(lines) == 0 {
		return "(no warning event explaining the failure)"
	}
	return strings.Join(lines, "\n")
}

// Creates one claimant and waits for CNI to either give it an address or
// refuse. Returns the address, empty on refusal.
func claimOneVM(name string) string {
	allClaimantVMs = append(allClaimantVMs, name)
	createClaimantVM(name)
	deadline := time.Now().Add(claimTimeout)
	for time.Now().Before(deadline) {
		if ip := podSecondaryIP(launcherPod(name)); ip != "" {
			return ip
		}
		time.Sleep(3 * time.Second)
	}
	return ""
}

func usableAddresses() int {
	_, ipNet, err := net.ParseCIDR(testRange)
	Expect(err).NotTo(HaveOccurred(), "TEST_RANGE %q is not a CIDR", testRange)
	ones, bits := ipNet.Mask.Size()
	Expect(bits).To(Equal(32), "TEST_RANGE must be IPv4")
	return (1 << (bits - ones)) - 2
}

// The refusal must name address exhaustion, in whatever words the plugin
// uses; a generic CNI failure forces the operator to guess.
var exhaustionMessage = regexp.MustCompile(
	`(?i)(no ip addresses available|no available addresses|could not allocate|exhaust|range is full|no free|out of addresses|address pool)`)

var _ = Describe("VM Secondary-Network IPAM", Label("pass-fail"), func() {
	var granted []string
	var refusalReason string

	It("should create the secondary network", func() {
		createNAD()
	})

	It("should fill the pool to exactly its usable size and refuse the next claim", func() {
		usable := usableAddresses()
		Expect(maxClaimants).To(BeNumerically(">", usable),
			"MAX_CLAIMANTS must exceed the pool size or exhaustion can never be reached")

		refused := ""
		for i := 1; i <= maxClaimants; i++ {
			name := fmt.Sprintf("%s-c%d", claimantPrefix, i)
			ip := claimOneVM(name)
			if ip == "" {
				refused = name
				refusalReason = podFailureEvents(launcherPod(name))
				AddReportEntry("refused_at_claimant", i)
				AddReportEntry("refusal_reason", refusalReason)
				break
			}
			granted = append(granted, ip)
			claimants = append(claimants, name)
		}

		Expect(granted).NotTo(BeEmpty(), "no claimant received an address; last failure:\n%s", refusalReason)
		AddReportEntry("granted_addresses", granted)
		Expect(len(granted)).To(Equal(usable),
			"expected exactly %d usable addresses to be granted, got %d; fewer means addresses were "+
				"already lost, more means the plugin hands out network or broadcast addresses",
			usable, len(granted))
		Expect(refusalReason).NotTo(BeEmpty(),
			"every claimant up to MAX_CLAIMANTS=%d got an address; exhaustion was never reached", maxClaimants)

		// The refused claimant holds no address but retries forever and would
		// take the next freed one; its job of proving the refusal is done.
		if refused != "" {
			deleteVM(refused)
			Eventually(func() int {
				pods, err := readerClientset().CoreV1().Pods(namespace).List(context.TODO(),
					metav1.ListOptions{LabelSelector: "vm.kubevirt.io/name=" + refused})
				Expect(err).NotTo(HaveOccurred(), "listing the refused claimant's pods")
				return len(pods.Items)
			}, 2*time.Minute, 5*time.Second).Should(BeZero(),
				"the refused claimant's pod is still retrying; it would steal the next freed address")
		}
	})

	It("should name address exhaustion in the refusal", func() {
		Expect(exhaustionMessage.FindString(refusalReason)).NotTo(BeEmpty(),
			"the refusal does not say the pool is empty, so an operator has to guess:\n%s", refusalReason)
	})

	It("should refuse a container from the same exhausted pool", func() {
		container := claimantPrefix + "-container"
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        container,
				Labels:      map[string]string{"app.kubernetes.io/managed-by": "uat-harness", "uat/run": runID},
				Annotations: map[string]string{"k8s.v1.cni.cncf.io/networks": nadName},
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Tolerations: []corev1.Toleration{{
					Key: "nvidia.com/gpu.product", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
				}},
				Containers: []corev1.Container{{
					Name:    "idle",
					Image:   "registry.redhat.io/ubi9/ubi-minimal:latest",
					Command: []string{"sleep", "3600"},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{"memory": resource.MustParse("64Mi")},
					},
				}},
			},
		}
		_, err := readerClientset().CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "creating the container claimant")

		deadline := time.Now().Add(claimTimeout)
		ip := ""
		for time.Now().Before(deadline) {
			ip = podSecondaryIP(container)
			if ip != "" {
				break
			}
			time.Sleep(3 * time.Second)
		}
		Expect(ip).To(BeEmpty(),
			"the container got %s from a pool the VMs already filled, so the pools are separate", ip)
		AddReportEntry("container_refusal_reason", podFailureEvents(container))
	})

	It("should hand a freed address to the waiting container", func() {
		freed := claimants[0]
		deleteVM(freed)
		claimants = claimants[1:]

		container := claimantPrefix + "-container"
		deadline := time.Now().Add(claimTimeout)
		ip := ""
		for time.Now().Before(deadline) {
			ip = podSecondaryIP(container)
			if ip != "" {
				break
			}
			time.Sleep(3 * time.Second)
		}
		Expect(ip).NotTo(BeEmpty(),
			"the address the VM released never became usable within %s: a leak on graceful delete", claimTimeout)
		AddReportEntry("container_took_address", ip)
	})

	It("should reuse an address after a graceful delete", func() {
		Expect(readerClientset().CoreV1().Pods(namespace).Delete(context.TODO(),
			claimantPrefix+"-container", metav1.DeleteOptions{})).To(Succeed(),
			"deleting the container claimant")

		refill := claimantPrefix + "-refill"
		ip := claimOneVM(refill)
		Expect(ip).NotTo(BeEmpty(),
			"an address released by gracefully deleted objects was not reusable")
		claimants = append(claimants, refill)
	})

	It("should not leak an address when its launcher pod is force-deleted", func() {
		victim := claimants[0]
		for cycle := 1; cycle <= 2; cycle++ {
			pod := launcherPod(victim)
			Expect(pod).NotTo(BeEmpty(), "no launcher pod to force-delete for %s", victim)
			before := podSecondaryIP(pod)

			zero := int64(0)
			err := readerClientset().CoreV1().Pods(namespace).Delete(context.TODO(), pod,
				metav1.DeleteOptions{GracePeriodSeconds: &zero})
			if !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred(), "force-deleting %s", pod)
			}

			// runStrategy Always respawns the pod. If the old allocation
			// leaked, the new pod never gets an address of its own.
			var after string
			deadline := time.Now().Add(2 * claimTimeout)
			for time.Now().Before(deadline) {
				after = podSecondaryIP(launcherPod(victim))
				if after != "" && after != before {
					break
				}
				time.Sleep(3 * time.Second)
			}
			Expect(after).NotTo(BeEmpty(),
				"cycle %d: the respawned pod never received an address: the force-deleted pod's "+
					"allocation leaked and permanently shrank the pool", cycle)
			AddReportEntry(fmt.Sprintf("cycle_%d_addresses", cycle), fmt.Sprintf("%s -> %s", before, after))
		}
	})
})

var _ = AfterSuite(func() {
	// Claimants first, the network last: deleting the network while
	// claimants still hold addresses leaves the IPAM plugin with
	// allocations it cannot reconcile. Best effort; the next run's
	// preflight sweeps anything left behind.
	for _, name := range allClaimantVMs {
		deleteVM(name)
	}
	_ = readerClientset().CoreV1().Pods(namespace).Delete(context.TODO(),
		claimantPrefix+"-container", metav1.DeleteOptions{})
	deleteNAD()
})

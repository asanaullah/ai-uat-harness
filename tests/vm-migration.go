// Assisted by Claude Opus
package test

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Live migration moves a running VM to another node without rebooting it.
// The move counts only if the VMI keeps its identity, the boot id never
// changes, the marker survives, and the node changed. The heartbeat's loss
// of freshness during the move measures the guest-visible pause, recorded
// rather than asserted like every timing in this suite.

var (
	vmURL     string
	vmName    string
	runID     string
	namespace string
	readerCfg *rest.Config
)

func TestVMMigration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VM Live Migration Suite")
}

var _ = BeforeSuite(func() {
	vmURL = os.Getenv("VM_URL")
	vmName = os.Getenv("VM_NAME")
	runID = os.Getenv("RUN_ID")
	Expect(vmURL).NotTo(BeEmpty(), "VM_URL must be set")
	Expect(vmName).NotTo(BeEmpty(), "VM_NAME must be set")
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

func dynamicClient() dynamic.Interface {
	client, err := dynamic.NewForConfig(readerCfg)
	Expect(err).NotTo(HaveOccurred())
	return client
}

var vmiGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
var vmimGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstancemigrations"}

func vmiState() (uid, node string) {
	obj, err := dynamicClient().Resource(vmiGVR).Namespace(namespace).
		Get(context.TODO(), vmName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "reading the VMI")
	uid, _, _ = unstructured.NestedString(obj.Object, "metadata", "uid")
	node, _, _ = unstructured.NestedString(obj.Object, "status", "nodeName")
	Expect(uid).NotTo(BeEmpty(), "the VMI has no uid")
	Expect(node).NotTo(BeEmpty(), "the VMI is not placed on a node yet")
	return uid, node
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

// Samples how stale the guest's served timestamp gets and how long between
// successful reads; a migration pause grows both by roughly its length.
type heartbeat struct {
	mu       sync.Mutex
	maxStale time.Duration
	maxGap   time.Duration
	last     time.Time
	failures int
	stop     chan struct{}
	done     sync.WaitGroup
}

func (h *heartbeat) start() {
	h.stop = make(chan struct{})
	h.done.Add(1)
	go func() {
		defer h.done.Done()
		client := &http.Client{Timeout: 2 * time.Second}
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-h.stop:
				return
			case now := <-tick.C:
				resp, err := client.Get(vmURL + "/heartbeat")
				if err != nil {
					h.mu.Lock()
					h.failures++
					h.mu.Unlock()
					continue
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					continue
				}
				nanos, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
				if err != nil {
					continue
				}
				h.mu.Lock()
				if !h.last.IsZero() {
					if gap := now.Sub(h.last); gap > h.maxGap {
						h.maxGap = gap
					}
				}
				h.last = now
				if stale := now.Sub(time.Unix(0, nanos)); stale > h.maxStale {
					h.maxStale = stale
				}
				h.mu.Unlock()
			}
		}
	}()
}

func (h *heartbeat) stopSampling() time.Duration {
	close(h.stop)
	h.done.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxStale
}

var _ = Describe("VM Live Migration", Label("pass-fail"), func() {
	var beforeUID, beforeNode, beforeBoot string
	var baselineStale time.Duration

	It("should boot the guest from the shared volume", func() {
		start := time.Now()
		Eventually(func(g Gomega) {
			body, err := fetch("/marker")
			g.Expect(err).NotTo(HaveOccurred(), "guest not serving yet")
			g.Expect(string(body)).To(Equal(runID + " from vm-migration\n"),
				"the marker did not come from this run's guest: %s", string(body))
		}, 10*time.Minute, 15*time.Second).Should(Succeed())
		AddReportEntry("first_boot_seconds", time.Since(start).Seconds())
	})

	It("should record the running guest", func() {
		var err error
		var body []byte
		Eventually(func(g Gomega) {
			body, err = fetch("/boot-id")
			g.Expect(err).NotTo(HaveOccurred(), "guest not serving the boot id")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
		beforeBoot = strings.TrimSpace(string(body))
		Expect(beforeBoot).NotTo(BeEmpty())
		beforeUID, beforeNode = vmiState()

		// Baseline heartbeat freshness before anything disturbs the guest.
		hb := &heartbeat{}
		hb.start()
		time.Sleep(5 * time.Second)
		baselineStale = hb.stopSampling()
		AddReportEntry("baseline_staleness_seconds", baselineStale.Seconds())
	})

	It("should migrate the VM to another node", func() {
		migration := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachineInstanceMigration",
			"metadata": map[string]interface{}{
				"name": vmName + "-migration",
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "uat-harness",
					"uat/run":                      runID,
				},
			},
			"spec": map[string]interface{}{"vmiName": vmName},
		}}
		_, err := dynamicClient().Resource(vmimGVR).Namespace(namespace).
			Create(context.TODO(), migration, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "creating the migration object")

		hb := &heartbeat{}
		hb.start()
		start := time.Now()
		Eventually(func(g Gomega) {
			obj, err := dynamicClient().Resource(vmimGVR).Namespace(namespace).
				Get(context.TODO(), vmName+"-migration", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "reading the migration object")
			phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
			g.Expect(phase).To(Equal("Succeeded"),
				"the migration has not succeeded; phase %q, object:\n%v", phase, obj.Object)
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		duringStale := hb.stopSampling()
		AddReportEntry("migration_seconds", time.Since(start).Seconds())
		AddReportEntry("pause_estimate_seconds", (duringStale - baselineStale).Seconds())
		AddReportEntry("heartbeat_read_gaps_seconds", hb.maxGap.Seconds())
		AddReportEntry("heartbeat_failures", hb.failures)
	})

	It("should have moved without rebooting", func() {
		afterUID, afterNode := vmiState()
		Expect(afterUID).To(Equal(beforeUID),
			"the VMI was replaced instead of migrated: %s became %s", beforeUID, afterUID)
		Expect(afterNode).NotTo(Equal(beforeNode),
			"the VMI never left node %s; the migration moved nothing", beforeNode)

		var body []byte
		Eventually(func(g Gomega) {
			var err error
			body, err = fetch("/marker")
			g.Expect(err).NotTo(HaveOccurred(), "guest not serving after the migration")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
		Expect(string(body)).To(Equal(runID + " from vm-migration\n"),
			"the marker changed across the migration: %s", string(body))

		Eventually(func(g Gomega) {
			var err error
			body, err = fetch("/boot-id")
			g.Expect(err).NotTo(HaveOccurred(), "guest not serving the boot id")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
		Expect(strings.TrimSpace(string(body))).To(Equal(beforeBoot),
			"the boot id changed: the guest rebooted, so the migration was not live")
	})
})

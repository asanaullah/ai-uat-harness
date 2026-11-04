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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// A containerDisk is thrown away with the pod, so it proves nothing about
// storage. This test gives the VM a CDI root disk, writes a marker on first
// boot, restarts the VM, and reads the marker back; the boot id changing
// proves the guest rebooted, so the surviving marker is persistence.

var (
	vmURL     string
	vmName    string
	runID     string
	namespace string
	readerCfg *rest.Config
)

func TestVMStorage(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VM Storage Suite")
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

// The identity the harness playbooks run under, which holds the project's
// user role: it may stop and start the VM through the subresources, which
// is what virtctl calls underneath.
func vmAction(action string) error {
	cfg := rest.CopyConfig(readerCfg)
	cfg.GroupVersion = &schema.GroupVersion{Group: "subresources.kubevirt.io", Version: "v1"}
	cfg.APIPath = "/apis"
	cfg.NegotiatedSerializer = serializer.NewCodecFactory(kscheme.Scheme).WithoutConversion()
	client, err := rest.RESTClientFor(cfg)
	if err != nil {
		return err
	}
	return client.Put().
		Namespace(namespace).
		Resource("virtualmachines").
		Name(vmName).
		SubResource(action).
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

var _ = Describe("VM Persistent Storage", Label("pass-fail"), func() {
	var firstBoot string

	It("should boot the guest from the volume", func() {
		// CDI imports the image into the PVC first; only then can the VM boot.
		start := time.Now()
		Eventually(func(g Gomega) {
			body, err := fetch("/marker")
			g.Expect(err).NotTo(HaveOccurred(), "guest not serving yet")
			g.Expect(string(body)).To(Equal(runID + " from vm-storage\n"),
				"the marker did not come from this run's guest: %s", string(body))
		}, 10*time.Minute, 15*time.Second).Should(Succeed())
		AddReportEntry("first_boot_seconds", time.Since(start).Seconds())
	})

	It("should record the first boot id", func() {
		body, err := fetch("/boot-id")
		Expect(err).NotTo(HaveOccurred())
		firstBoot = strings.TrimSpace(string(body))
		Expect(firstBoot).NotTo(BeEmpty(), "the guest served no boot id")
	})

	It("should stop the VM", func() {
		Expect(vmAction("stop")).NotTo(HaveOccurred(), "asking the VM to stop")
		Eventually(func() bool {
			_, err := fetch("/marker")
			return err != nil
		}, 3*time.Minute, 5*time.Second).Should(BeTrue(), "the guest is still serving after the stop")
	})

	It("should start the VM again and keep the marker", func() {
		Expect(vmAction("start")).NotTo(HaveOccurred(), "asking the VM to start")
		Eventually(func(g Gomega) {
			body, err := fetch("/marker")
			g.Expect(err).NotTo(HaveOccurred(), "guest not serving after the restart")
			g.Expect(string(body)).To(Equal(runID + " from vm-storage\n"),
				"the marker changed across the restart: the disk did not persist it: %s", string(body))
		}, 6*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("should have rebooted, not resumed", func() {
		var secondBoot string
		Eventually(func(g Gomega) {
			body, err := fetch("/boot-id")
			g.Expect(err).NotTo(HaveOccurred(), "guest not serving the boot id")
			secondBoot = strings.TrimSpace(string(body))
			g.Expect(secondBoot).NotTo(BeEmpty())
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
		Expect(secondBoot).NotTo(Equal(firstBoot),
			"the boot id did not change: the guest never really rebooted, so the marker surviving proves nothing")
	})
})

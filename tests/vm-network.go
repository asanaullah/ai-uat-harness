// Assisted by Claude Opus
package test

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A KubeVirt VM should be reachable from its namespace and able to reach
// out itself. The guest serves a marker over HTTP and runs its own probes,
// publishing the results next to the marker.

var (
	vmURL          string
	runID          string
	expectedProbes int
)

func TestVMNetwork(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VM Network Suite")
}

var _ = BeforeSuite(func() {
	vmURL = os.Getenv("VM_URL")
	runID = os.Getenv("RUN_ID")
	raw := os.Getenv("EXPECTED_PROBES")
	var err error
	expectedProbes, err = strconv.Atoi(raw)
	Expect(err).NotTo(HaveOccurred(), "EXPECTED_PROBES must be a number, got %q", raw)
	Expect(vmURL).NotTo(BeEmpty(), "VM_URL must be set")
	Expect(runID).NotTo(BeEmpty(), "RUN_ID must be set")
	Expect(expectedProbes).To(BeNumerically(">", 0), "EXPECTED_PROBES must be set")
})

func httpGet(g Gomega, url string) []byte {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	g.Expect(err).NotTo(HaveOccurred(), "reaching %s", url)
	defer resp.Body.Close()
	g.Expect(resp.StatusCode).To(Equal(200), "fetching %s", url)
	body, err := io.ReadAll(resp.Body)
	g.Expect(err).NotTo(HaveOccurred())
	return body
}

var _ = Describe("VM Networking", Label("pass-fail"), func() {
	It("should serve the guest's marker through its service", func() {
		// The guest takes minutes to boot, pull included.
		Eventually(func(g Gomega) {
			body := httpGet(g, vmURL+"/marker")
			g.Expect(string(body)).To(Equal(runID+" from vm-network\n"),
				"the marker did not come from this run's guest: %s", string(body))
		}, 6*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("should reach every target the guest probed", func() {
		// The probes start with the HTTP server, so the file arrives last.
		Eventually(func(g Gomega) {
			body := httpGet(g, vmURL+"/probes.txt")
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			g.Expect(len(lines)).To(Equal(expectedProbes),
				"the guest's probe run is incomplete:\n%s", string(body))
			for _, line := range lines {
				g.Expect(strings.HasPrefix(line, "ok ")).To(BeTrue(),
					"a guest probe failed:\n%s", string(body))
			}
		}, 2*time.Minute, 10*time.Second).Should(Succeed())
	})
})

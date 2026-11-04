// Assisted by Claude Opus
package test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The results claim is ReadWriteMany on NFS. This test proves the share is
// real across nodes: the writer pod on one node leaves a marker in the
// scratchpad, and this pod, pinned to a different node, reads it back.

var (
	runID      string
	writerNode string
	ownNode    string
	markerDir  string
)

func TestRWX(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "RWX Shared Storage Suite")
}

var _ = BeforeSuite(func() {
	runID = os.Getenv("RUN_ID")
	writerNode = os.Getenv("WRITER_NODE")
	ownNode = os.Getenv("NODE")
	markerDir = os.Getenv("MARKER_DIR")
	Expect(runID).NotTo(BeEmpty(), "RUN_ID must be set")
	Expect(writerNode).NotTo(BeEmpty(), "WRITER_NODE must be set")
	Expect(ownNode).NotTo(BeEmpty(), "NODE must be set")
	Expect(markerDir).NotTo(BeEmpty(), "MARKER_DIR must be set")
})

var _ = Describe("RWX Shared Storage", Label("pass-fail"), func() {
	It("should run on a different node than the writer", func() {
		Expect(ownNode).NotTo(Equal(writerNode),
			"this pod landed on %s, the same node the marker was written from", ownNode)
	})

	It("should read what the other node wrote", func() {
		content, err := os.ReadFile(markerDir + "/marker-" + writerNode)
		Expect(err).NotTo(HaveOccurred(),
			"marker-%s is not in %s: the write from node %s did not reach this node",
			writerNode, markerDir, writerNode)
		Expect(string(content)).To(Equal(runID+" from "+writerNode+"\n"),
			"the marker holds something other than what %s wrote", writerNode)
	})
})

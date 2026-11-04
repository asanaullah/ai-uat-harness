// Assisted by Claude Opus
package test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Metrics from every namespace land in one shared pile that nothing in the
// pipeline separates. The separation sits at the doors a project can use: the
// tenancy port checks identity per namespace, the web port is cluster-only.
// This test publishes a canary from the peer and tries every way to read it.

var (
	metricName  string
	runID       string
	canaryURL   string
	tenancyURL  string
	webURL      string
	peerSpace   string
	readerFile  string
	queryClient *http.Client
)

func TestMetricsIsolation(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Metrics Isolation Suite")
}

var _ = BeforeSuite(func() {
	metricName = os.Getenv("METRIC_NAME")
	runID = os.Getenv("RUN_ID")
	canaryURL = os.Getenv("CANARY_URL")
	tenancyURL = os.Getenv("THANOS_TENANCY_URL")
	peerSpace = os.Getenv("PEER_NAMESPACE")
	webURL = os.Getenv("THANOS_WEB_URL")
	readerFile = os.Getenv("READER_TOKEN_FILE")
	queryClient = httpClient()
	Expect(metricName).NotTo(BeEmpty(), "METRIC_NAME must be set")
	Expect(runID).NotTo(BeEmpty(), "RUN_ID must be set")
	Expect(canaryURL).NotTo(BeEmpty(), "CANARY_URL must be set")
	Expect(tenancyURL).NotTo(BeEmpty(), "THANOS_TENANCY_URL must be set")
})

// The identity these checks query as: the one the playbooks run under, with
// the project's user role but not the peer's metrics read.
func readerConfig() *rest.Config {
	cfg, err := rest.InClusterConfig()
	Expect(err).NotTo(HaveOccurred(), "reading the pod's own service account")
	Expect(readerFile).NotTo(BeEmpty(), "READER_TOKEN_FILE must be set")
	token, err := os.ReadFile(readerFile)
	Expect(err).NotTo(HaveOccurred(), "reading the reader token")
	cfg.BearerToken = strings.TrimSpace(string(token))
	cfg.BearerTokenFile = ""
	return cfg
}

func ownNamespace() string {
	raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	Expect(err).NotTo(HaveOccurred(), "reading the pod's namespace")
	return strings.TrimSpace(string(raw))
}

// Thanos Querier serves TLS with the service CA; the test checks
// authorization, not server identity.
func httpClient() *http.Client {
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	if ca, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"); err == nil {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(ca) {
			transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
		}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}
}

func ownTokenConfig() *rest.Config {
	cfg, err := rest.InClusterConfig()
	Expect(err).NotTo(HaveOccurred(), "reading the pod's own service account")
	return cfg
}

func mayReadMetrics(cfg *rest.Config, namespace string) bool {
	clientset, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	review, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(
		context.TODO(),
		&authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Verb: "get", Group: "metrics.k8s.io", Resource: "pods", Namespace: namespace,
				},
			},
		},
		metav1.CreateOptions{},
	)
	Expect(err).NotTo(HaveOccurred())
	return review.Status.Allowed
}

type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		} `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

func thanosQuery(base, token, namespace, query string) (int, *queryResponse, string) {
	values := url.Values{}
	values.Set("query", query)
	if namespace != "" {
		values.Set("namespace", namespace)
	}
	request, err := http.NewRequest("GET", base+"/api/v1/query?"+values.Encode(), nil)
	Expect(err).NotTo(HaveOccurred())
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := queryClient.Do(request)
	Expect(err).NotTo(HaveOccurred(), "reaching %s", base)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	Expect(err).NotTo(HaveOccurred())
	parsed := &queryResponse{}
	_ = json.Unmarshal(body, parsed)
	return response.StatusCode, parsed, string(body)
}

func canarySelector() string {
	return metricName + `{run="` + runID + `"}`
}

func namespacesOf(response *queryResponse) []string {
	found := []string{}
	for _, result := range response.Data.Result {
		found = append(found, result.Metric["namespace"])
	}
	return found
}

var _ = Describe("Project Namespace Metrics", Label("project-check"), func() {
	var own, peer, reader string
	var readerCfg, ownCfg *rest.Config

	BeforeEach(func() {
		own = ownNamespace()
		Expect(peerSpace).NotTo(BeEmpty(), "PEER_NAMESPACE must be set")
		Expect(webURL).NotTo(BeEmpty(), "THANOS_WEB_URL must be set")
		peer = peerSpace
		readerCfg = readerConfig()
		ownCfg = ownTokenConfig()
		token, err := os.ReadFile(readerFile)
		Expect(err).NotTo(HaveOccurred())
		reader = strings.TrimSpace(string(token))
	})

	It("should serve the canary at the source", func() {
		response, err := queryClient.Get(canaryURL + "/metrics")
		Expect(err).NotTo(HaveOccurred())
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(response.StatusCode).To(Equal(200), "canary at %s", canaryURL)
		Expect(string(body)).To(ContainSubstring(metricName + `{run="` + runID + `"}`),
			"the canary must serve %s{run=%q}: %s", metricName, runID, string(body))
	})

	It("should let the reader read this namespace's metrics", func() {
		Expect(mayReadMetrics(readerCfg, own)).To(BeTrue(),
			"the reader may not read metrics in %s; the tenancy port would refuse every query, "+
				"including this namespace's own canary. Run through the runner pod (-p), or as a "+
				"member of the project group.", own)
	})

	It("should not let the reader read the peer namespace's metrics", func() {
		Expect(mayReadMetrics(readerCfg, peer)).To(BeFalse(),
			"the reader may read metrics in %s, so it is not a plain tenant of %s; "+
				"the peer's canary would be readable through the tenancy port. Run through the "+
				"runner pod (-p), or as a member of the project group.", peer, own)
	})

	It("should serve this namespace's canary through the tenancy port", func() {
		// The positive control: the canary reaches the shared pile, and an
		// authorized reader gets it back. Every refusal below is only
		// meaningful because this one succeeds.
		Eventually(func(g Gomega) {
			code, response, body := thanosQuery(tenancyURL, reader, own, canarySelector())
			g.Expect(code).To(Equal(200), "tenancy port: %s", body)
			g.Expect(namespacesOf(response)).To(ContainElement(own),
				"tenancy port returned no %s canary yet: %s", own, body)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("should keep the peer's canary out of this namespace's queries", func() {
		// What a project's Grafana does: query the tenancy port with its own
		// namespace. The port pins the namespace into the query server-side,
		// so none of these shapes may return the peer's series.
		shapes := []struct {
			name        string
			query       string
			mustSucceed bool
			sum         bool
		}{
			{"no namespace matcher", canarySelector(), true, false},
			{"explicit peer matcher", metricName + `{run="` + runID + `",namespace="` + peer + `"}`, false, false},
			{"regex matcher", metricName + `{run="` + runID + `",namespace=~".+"}`, false, false},
			{"aggregation", "sum(" + canarySelector() + ")", true, true},
		}
		for _, shape := range shapes {
			code, response, body := thanosQuery(tenancyURL, reader, own, shape.query)
			if shape.mustSucceed {
				Expect(code).To(Equal(200), "query with %s: %s", shape.name, body)
			}
			if code != 200 {
				fmt.Printf("query with %s returned %d: %s\n", shape.name, code, body)
				continue
			}
			Expect(response.Status).To(Equal("success"), "query with %s: %s", shape.name, body)
			if shape.sum {
				Expect(response.Data.Result).To(HaveLen(1), "query with %s: %s", shape.name, body)
				value := fmt.Sprintf("%v", response.Data.Result[0].Value[1])
				Expect(value).To(Equal("1"),
					"sum over %s counted %s series; the peer's canary is in the result", shape.name, value)
			} else {
				for _, ns := range namespacesOf(response) {
					Expect(ns).To(Equal(own),
						"query with %s returned the canary of %s: %s", shape.name, ns, body)
				}
			}
		}
	})

	It("should refuse the peer's namespace on the tenancy port", func() {
		code, _, body := thanosQuery(tenancyURL, reader, peer, canarySelector())
		Expect(code).To(Equal(403),
			"asking for %s with this identity must be refused; got %d: %s", peer, code, body)
	})

	It("should refuse the reader on the web port", func() {
		// The shared pile, unpinned: only cluster monitors may read it.
		code, _, body := thanosQuery(webURL, reader, "", canarySelector())
		Expect(code).To(Equal(403),
			"the web port must refuse a tenant token; got %d: %s", code, body)
	})

	It("should refuse a plain tenant pod on the tenancy port", func() {
		// The check pod's own service account holds no role, so even this
		// namespace's metrics stay closed to it.
		code, _, body := thanosQuery(tenancyURL, ownToken(ownCfg), own, canarySelector())
		Expect(code).To(Equal(403),
			"the tenancy port must demand metrics read rights; got %d: %s", code, body)
	})
})

var _ = Describe("Peer Namespace Metrics", Label("peer-check"), func() {
	It("should serve the canary at the source", func() {
		response, err := queryClient.Get(canaryURL + "/metrics")
		Expect(err).NotTo(HaveOccurred())
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(response.StatusCode).To(Equal(200), "canary at %s", canaryURL)
		Expect(string(body)).To(ContainSubstring(metricName + `{run="` + runID + `"}`),
			"the canary must serve %s{run=%q}: %s", metricName, runID, string(body))
	})

	It("should refuse a plain tenant pod on the tenancy port", func() {
		// The peer's role grants no metrics read, so not even the peer's own
		// canary comes back through the tenancy port from here.
		own := ownNamespace()
		code, _, body := thanosQuery(tenancyURL, ownToken(ownTokenConfig()), own, canarySelector())
		Expect(code).To(Equal(403),
			"the tenancy port must demand metrics read rights; got %d: %s", code, body)
	})
})

func ownToken(cfg *rest.Config) string {
	token, err := os.ReadFile(cfg.BearerTokenFile)
	Expect(err).NotTo(HaveOccurred(), "reading the pod's own token")
	return strings.TrimSpace(string(token))
}

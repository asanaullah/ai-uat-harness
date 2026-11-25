// Assisted by Claude Opus
package test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type hubCluster struct {
	name      string
	namespace string
}

var (
	hubClientset     *kubernetes.Clientset
	hubDynamicClient dynamic.Interface
	hubClusterGVR    schema.GroupVersionResource
	hubClusters      []hubCluster
	isHub            bool
)

func TestComplianceHub(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Compliance Hub Test Suite")
}

var _ = BeforeSuite(func() {
	config, err := rest.InClusterConfig()
	Expect(err).NotTo(HaveOccurred())
	hubClientset, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
	hubDynamicClient, err = dynamic.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())

	ctx := context.Background()
	for _, version := range []string{"v1beta1", "v1alpha1"} {
		gvr := schema.GroupVersionResource{Group: "hypershift.openshift.io", Version: version, Resource: "hostedclusters"}
		list, err := hubDynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{})
		if isMissingHub(err) {
			continue
		}
		Expect(err).NotTo(HaveOccurred())
		hubClusterGVR = gvr
		if list == nil || len(list.Items) == 0 {
			fmt.Println("not a hub: no HostedClusters")
			return
		}
		for _, item := range list.Items {
			hubClusters = append(hubClusters, hubCluster{name: item.GetName(), namespace: item.GetNamespace()})
		}
		sort.Slice(hubClusters, func(i, j int) bool { return hubClusters[i].name < hubClusters[j].name })
		isHub = true
		fmt.Printf("hosted clusters: %v\n", hubClusterNames())
		return
	}
	fmt.Println("not a hub: no HostedCluster API")
})

func hubClusterNames() []string {
	out := make([]string, 0, len(hubClusters))
	for _, cluster := range hubClusters {
		out = append(out, cluster.name)
	}
	return out
}

func isMissingHub(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "not found") ||
		strings.Contains(message, "could not find the requested resource") ||
		strings.Contains(message, "no matches for kind")
}

func hubConditionStatus(obj map[string]interface{}, conditionType string) string {
	conditions, found, err := unstructured.NestedSlice(obj, "status", "conditions")
	if err != nil || !found {
		return ""
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(condition, "type")
		if t != conditionType {
			continue
		}
		status, _, _ := unstructured.NestedString(condition, "status")
		return status
	}
	return ""
}

var _ = Describe("Compliance hub", Label("pass-fail"), func() {
	BeforeEach(func() {
		if !isHub {
			Skip("not a hub (no HostedCluster API or no HostedClusters)")
		}
	})

	It("H-01 HostedClusters are available", func() {
		ctx := context.Background()
		var failures []string
		for _, cluster := range hubClusters {
			obj, err := hubDynamicClient.Resource(hubClusterGVR).Namespace(cluster.namespace).Get(ctx, cluster.name, metav1.GetOptions{})
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s read HostedCluster: %v", cluster.name, err))
				continue
			}
			if available := hubConditionStatus(obj.Object, "Available"); available != "True" {
				failures = append(failures, fmt.Sprintf("%s HostedCluster Available=%q", cluster.name, available))
			} else {
				fmt.Printf("PASS HostedCluster %s Available\n", cluster.name)
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("H-02 hosted apiserver audit path", func() {
		ctx := context.Background()
		var failures []string
		for _, cluster := range hubClusters {
			namespace := "clusters-" + cluster.name
			cm, err := hubClientset.CoreV1().ConfigMaps(namespace).Get(ctx, "openshift-apiserver", metav1.GetOptions{})
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s read %s/openshift-apiserver: %v", cluster.name, namespace, err))
				continue
			}
			var cfg struct {
				APIServerArguments map[string][]string `yaml:"apiServerArguments"`
			}
			if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &cfg); err != nil {
				failures = append(failures, fmt.Sprintf("%s parse %s/openshift-apiserver config.yaml: %v", cluster.name, namespace, err))
				continue
			}
			path := ""
			if values := cfg.APIServerArguments["audit-log-path"]; len(values) > 0 {
				path = values[0]
			}
			if path != "/var/log/openshift-apiserver/audit.log" {
				failures = append(failures, fmt.Sprintf("%s audit-log-path=%q", cluster.name, path))
			} else {
				fmt.Printf("PASS %s audit-log-path=%s\n", cluster.name, path)
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("H-03 hub ACS Central is available", func() {
		ctx := context.Background()
		gvr := schema.GroupVersionResource{Group: "platform.stackrox.io", Version: "v1alpha1", Resource: "centrals"}
		central, err := hubDynamicClient.Resource(gvr).Namespace("stackrox").Get(ctx, "stackrox-central-services", metav1.GetOptions{})
		var failures []string
		switch {
		case isMissingHub(err):
			failures = append(failures, "no Central API or Central object on the hub")
		case err != nil:
			failures = append(failures, fmt.Sprintf("read Central: %v", err))
		default:
			if available := hubConditionStatus(central.Object, "Available"); available != "True" {
				failures = append(failures, fmt.Sprintf("Central Available=%q", available))
			} else {
				fmt.Println("PASS hub Central Available")
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("H-04 hosted etcd backups exist", func() {
		ctx := context.Background()
		var failures []string
		for _, cluster := range hubClusters {
			found := ""
			hcpNamespace := "clusters-" + cluster.name
			cronjobs, err := hubClientset.BatchV1().CronJobs(hcpNamespace).List(ctx, metav1.ListOptions{})
			if err != nil && !isMissingHub(err) {
				failures = append(failures, fmt.Sprintf("%s list CronJobs in %s: %v", cluster.name, hcpNamespace, err))
				continue
			}
			if cronjobs != nil {
				for _, cronjob := range cronjobs.Items {
					if strings.Contains(strings.ToLower(cronjob.Name), "etcd") {
						found = hcpNamespace + "/" + cronjob.Name
						break
					}
				}
			}
			if found == "" {
				shared, err := hubClientset.BatchV1().CronJobs("etcd-backup").List(ctx, metav1.ListOptions{})
				if err != nil && !isMissingHub(err) {
					failures = append(failures, fmt.Sprintf("%s list CronJobs in etcd-backup: %v", cluster.name, err))
					continue
				}
				if shared != nil {
					for _, cronjob := range shared.Items {
						name := strings.ToLower(cronjob.Name)
						if strings.Contains(name, strings.ToLower(cluster.name)) && strings.Contains(name, "etcd") {
							found = "etcd-backup/" + cronjob.Name
							break
						}
					}
				}
			}
			if found == "" {
				failures = append(failures, fmt.Sprintf("%s: no etcd backup CronJob on the hub", cluster.name))
			} else {
				fmt.Printf("PASS %s etcd backup %s\n", cluster.name, found)
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("H-05 HostedCluster OAuth token settings", func() {
		ctx := context.Background()
		var failures []string
		for _, cluster := range hubClusters {
			obj, err := hubDynamicClient.Resource(hubClusterGVR).Namespace(cluster.namespace).Get(ctx, cluster.name, metav1.GetOptions{})
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s read HostedCluster: %v", cluster.name, err))
				continue
			}
			age, foundAge, _ := unstructured.NestedInt64(obj.Object, "spec", "configuration", "oauth", "tokenConfig", "accessTokenMaxAgeSeconds")
			idle, foundIdle, _ := unstructured.NestedString(obj.Object, "spec", "configuration", "oauth", "tokenConfig", "accessTokenInactivityTimeout")
			if !foundAge || age <= 0 || age > 86400 {
				failures = append(failures, fmt.Sprintf("%s accessTokenMaxAgeSeconds=%d (want <= 86400)", cluster.name, age))
			}
			if !foundIdle || idle == "" {
				failures = append(failures, fmt.Sprintf("%s accessTokenInactivityTimeout unset", cluster.name))
			}
			if foundAge && age > 0 && age <= 86400 && foundIdle && idle != "" {
				fmt.Printf("PASS %s OAuth tokens %ds, idle %s\n", cluster.name, age, idle)
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("H-06 Hosted NodePools have their requested nodes", func() {
		ctx := context.Background()
		var failures []string
		for _, cluster := range hubClusters {
			gvr := schema.GroupVersionResource{
				Group:    hubClusterGVR.Group,
				Version:  hubClusterGVR.Version,
				Resource: "nodepools",
			}
			pools, err := hubDynamicClient.Resource(gvr).Namespace(cluster.namespace).List(ctx, metav1.ListOptions{})
			if isMissingHub(err) {
				failures = append(failures, fmt.Sprintf("%s: no NodePool API", cluster.name))
				continue
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s list NodePools: %v", cluster.name, err))
				continue
			}
			if pools == nil {
				failures = append(failures, fmt.Sprintf("%s: no NodePools", cluster.name))
				continue
			}
			count := 0
			for _, pool := range pools.Items {
				clusterName, _, _ := unstructured.NestedString(pool.Object, "spec", "clusterName")
				if clusterName != "" && clusterName != cluster.name {
					continue
				}
				count++
				desired, found, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
				if !found {
					desired = 1
				}
				have, _, _ := unstructured.NestedInt64(pool.Object, "status", "replicas")
				if have != desired {
					failures = append(failures, fmt.Sprintf("%s NodePool %s has %d of %d nodes", cluster.name, pool.GetName(), have, desired))
				} else {
					fmt.Printf("PASS %s NodePool %s has %d of %d nodes\n", cluster.name, pool.GetName(), have, desired)
				}
			}
			if count == 0 {
				failures = append(failures, fmt.Sprintf("%s has no NodePool", cluster.name))
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})
})

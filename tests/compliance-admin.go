// Assisted by Claude Opus
package test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	clientset            *kubernetes.Clientset
	dynamicClient        dynamic.Interface
	adminNS              string
	probeImage           string
	probeSA              string
	probeLogs            map[string]string
	probeErrs            map[string]string
	nodeNames            []string
	allowedSCCPatterns   []*regexp.Regexp
	scanBaseline         map[string]int
	scanMaxAgeHours      int
	backupMaxAgeHours    int
	cliImage             string
	isHostedCluster      bool
	platformType         string
	apiServerHost        string
	deviceResource       string
	fipsRhcos            string
	fipsProviderVersion  string
	gpuClaimAllowRe      *regexp.Regexp
	operatorFipsAccepted map[string]string
	networkPolicyExempt  *regexp.Regexp
	memclearRecord       map[string]string
)

func TestComplianceAdmin(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Compliance Admin Test Suite")
}

const nodeProbe = `
echo "NODE=$NODE_NAME"
chroot /host /bin/bash -c '
echo "FIPS=$(cat /proc/sys/crypto/fips_enabled 2>/dev/null || echo unknown)"
echo "CMDLINE_FIPS=$(grep -o "fips=1" /proc/cmdline || echo MISSING)"
echo "POLICY=$(update-crypto-policies --show 2>/dev/null || echo unknown)"
echo "CHRONYD=$(systemctl is-active chronyd 2>/dev/null || echo unknown)"
echo "NTPSYNC=$(timedatectl show -p NTPSynchronized --value 2>/dev/null || echo unknown)"
echo "NOUSB=$(tr " " "\n" </proc/cmdline | grep -cx nousb)"
echo "USBGUARD=$(systemctl is-active usbguard 2>/dev/null || echo unknown)"
echo "VFIO_IOMMU_TYPE1=$( [ -d /sys/module/vfio_iommu_type1 ] && echo loaded || echo missing )"
echo "IOMMU_GROUPS=$(ls /sys/kernel/iommu_groups 2>/dev/null | wc -l)"
echo "IOMMU_OFF=$(grep -oE "(amd|intel)_iommu=off" /proc/cmdline | head -1)"
for d in /sys/bus/pci/devices/*; do
  [ "$(cat "$d/vendor" 2>/dev/null)" = 0x10de ] || continue
  case "$(cat "$d/class" 2>/dev/null)" in 0x030*) ;; *) continue ;; esac
  echo "GPU=$(cat "$d/device" 2>/dev/null) $(cat "$d/reset_method" 2>/dev/null)"
done
echo "NFS_TLS=$(awk "\$3 ~ /^nfs/ && /xprtsec=tls/ {n++} END{print n+0}" /proc/mounts)"
echo "NFS_CLEAR=$(awk "\$3 ~ /^nfs/ && !/xprtsec=tls/ {n++} END{print n+0}" /proc/mounts)"
echo "FIPS_PROVIDER=$(openssl list -providers 2>/dev/null | awk "/fips/{f=1} f&&/version:/{print \$2; exit}")"
udevadm settle 2>/dev/null
for d in $(lsblk -rpno NAME,FSTYPE | awk "\$2==\"crypto_LUKS\"{print \$1}"); do
  v=$(cryptsetup luksDump "$d" 2>/dev/null | awk "/^Version:/{print \$2; exit}")
  t=$(clevis luks list -d "$d" 2>/dev/null | grep -c tang)
  echo "LUKS=$d v=$v tang=$t"
done
lsblk -rpno NAME,TYPE,FSTYPE,LABEL | awk "(\$2==\"disk\"||\$2==\"part\") && \$3!=\"\" && \$3!=\"crypto_LUKS\" && \$4!=\"boot\" && \$4!=\"EFI-SYSTEM\" {print \"PLAIN=\"\$1\" \"\$3}"
echo END=1
'
`

var _ = BeforeSuite(func() {
	adminNS = envOr("UAT_ADMIN_NAMESPACE", "uat-admin")
	probeImage = envOr("NODE_PROBE_IMAGE", "registry.access.redhat.com/ubi9/ubi:9.8")
	probeSA = envOr("NODE_PROBE_SERVICE_ACCOUNT", "uat-admin-sa")

	config, err := rest.InClusterConfig()
	Expect(err).NotTo(HaveOccurred())
	apiServerHost = config.Host
	clientset, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
	dynamicClient, err = dynamic.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
	allowedSCCPatterns, err = compileSCCPatterns()
	Expect(err).NotTo(HaveOccurred())

	scanMaxAgeHours = envInt("SCAN_MAX_AGE_HOURS", 48)
	backupMaxAgeHours = envInt("BACKUP_MAX_AGE_HOURS", 26)
	cliImage = os.Getenv("UAT_CLI_IMAGE")
	scanBaseline = map[string]int{}
	if raw := os.Getenv("SCAN_BASELINE"); raw != "" && raw != "{}" && raw != "null" {
		err := json.Unmarshal([]byte(raw), &scanBaseline)
		Expect(err).NotTo(HaveOccurred(), "parse SCAN_BASELINE")
	}
	if raw := os.Getenv("GPU_CLAIM_ALLOW_NS"); raw != "" {
		gpuClaimAllowRe, err = regexp.Compile(raw)
		Expect(err).NotTo(HaveOccurred(), "compile GPU_CLAIM_ALLOW_NS")
	}
	operatorFipsAccepted = map[string]string{}
	if raw := os.Getenv("OPERATOR_FIPS_ACCEPTED"); raw != "" && raw != "{}" && raw != "null" {
		err := json.Unmarshal([]byte(raw), &operatorFipsAccepted)
		Expect(err).NotTo(HaveOccurred(), "parse OPERATOR_FIPS_ACCEPTED")
	}
	exemptPattern := envOr("NETWORK_POLICY_EXEMPT_NS", `^(default|kube-.*|openshift-.*)$`)
	networkPolicyExempt, err = regexp.Compile(exemptPattern)
	Expect(err).NotTo(HaveOccurred(), "compile NETWORK_POLICY_EXEMPT_NS")
	memclearRecord = map[string]string{}
	if raw := os.Getenv("MEMCLEAR_RECORD"); raw != "" && raw != "{}" && raw != "null" {
		err := json.Unmarshal([]byte(raw), &memclearRecord)
		Expect(err).NotTo(HaveOccurred(), "parse MEMCLEAR_RECORD")
	}

	ctx := context.Background()
	infraGVR := schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "infrastructures"}
	infra, err := dynamicClient.Resource(infraGVR).Get(ctx, "cluster", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	topology, _, _ := unstructured.NestedString(infra.Object, "status", "controlPlaneTopology")
	isHostedCluster = topology == "External"
	platformType, _, _ = unstructured.NestedString(infra.Object, "status", "platformStatus", "type")
	fmt.Printf("controlPlaneTopology: %s platform: %s\n", topology, platformType)

	deviceResource = os.Getenv("DEVICE_RESOURCE")
	fipsRhcos = os.Getenv("FIPS_RHCOS")
	fipsProviderVersion = os.Getenv("FIPS_PROVIDER_VERSION")

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(nodes.Items).NotTo(BeEmpty())

	probeLogs = map[string]string{}
	probeErrs = map[string]string{}
	for _, node := range nodes.Items {
		nodeNames = append(nodeNames, node.Name)
	}
	sort.Strings(nodeNames)

	for _, node := range nodeNames {
		log, err := runProbe(node)
		if err != nil {
			probeErrs[node] = err.Error()
			continue
		}
		probeLogs[node] = log
	}

	for _, node := range nodeNames {
		fmt.Printf("--- %s ---\n", node)
		if err, ok := probeErrs[node]; ok {
			fmt.Printf("ERROR: %s\n", err)
			continue
		}
		fmt.Print(probeLogs[node])
	}
})

func runProbe(node string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	privileged := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "uat-compliance-probe-",
			Namespace:    adminNS,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "uat-harness",
				"uat/test":                     "compliance-admin",
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: probeSA,
			RestartPolicy:      corev1.RestartPolicyNever,
			NodeName:           node,
			HostPID:            true,
			Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           probeImage,
				Command:         []string{"/bin/sh", "-c", nodeProbe},
				Env:             []corev1.EnvVar{{Name: "NODE_NAME", Value: node}},
				SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
				VolumeMounts:    []corev1.VolumeMount{{Name: "host", MountPath: "/host"}},
			}},
			Volumes: []corev1.Volume{{
				Name: "host",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/"},
				},
			}},
		},
	}

	created, err := clientset.CoreV1().Pods(adminNS).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create probe: %w", err)
	}
	name := created.Name
	defer func() {
		_ = clientset.CoreV1().Pods(adminNS).Delete(context.Background(), name, metav1.DeleteOptions{})
	}()

	var phase corev1.PodPhase
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("probe %s did not finish: %w", name, ctx.Err())
		default:
		}
		p, err := clientset.CoreV1().Pods(adminNS).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get probe %s: %w", name, err)
		}
		phase = p.Status.Phase
		if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
			break
		}
		time.Sleep(2 * time.Second)
	}

	log, err := clientset.CoreV1().Pods(adminNS).GetLogs(name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("read probe %s log: %w", name, err)
	}
	out := string(log)
	if !strings.Contains(out, "END=1") {
		return "", fmt.Errorf("probe %s ended %s without END=1: %s", name, phase, out)
	}
	return out, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if raw := os.Getenv(key); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			return value
		}
	}
	return fallback
}

func hoursSince(t time.Time) int {
	return int(time.Since(t).Hours())
}

func values(log string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(log, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func linesWithPrefix(log, prefix string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func probeFailure(node string) (string, bool) {
	err, ok := probeErrs[node]
	return err, ok
}

func compileSCCPatterns() ([]*regexp.Regexp, error) {
	patterns := []string{`^nvidia-`, `^kubevirt-controller$`}
	raw := os.Getenv("ALLOWED_SCC_PATTERNS")
	if raw != "" && raw != "[]" {
		var parsed []string
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil, err
		}
		patterns = parsed
	}
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		out = append(out, re)
	}
	return out, nil
}

func isMissing(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "not found") ||
		strings.Contains(s, "could not find the requested resource") ||
		strings.Contains(s, "no matches for kind")
}

func hasString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func conditionStatus(obj map[string]interface{}, conditionType string) string {
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

func podReady(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return false
		}
	}
	return true
}

func pipelineInputRefs(obj map[string]interface{}) []string {
	pipelines, found, err := unstructured.NestedSlice(obj, "spec", "pipelines")
	if err != nil || !found {
		return nil
	}
	var out []string
	for _, raw := range pipelines {
		pipeline, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		refs, _, _ := unstructured.NestedStringSlice(pipeline, "inputRefs")
		out = append(out, refs...)
	}
	return out
}

type tlsTarget struct {
	name string
	host string
	port string
}

type tlsHandshakeProbe struct {
	name     string
	version  uint16
	suites   []uint16
	required bool
}

var legacyCipherSuites = []uint16{
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
}

var tlsHandshakeProbes = []tlsHandshakeProbe{
	{name: "tls13", version: tls.VersionTLS13, required: true},
	{name: "tls12-gcm", version: tls.VersionTLS12, required: true, suites: []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	}},
	{name: "tls10", version: tls.VersionTLS10, suites: legacyCipherSuites},
	{name: "tls11", version: tls.VersionTLS11, suites: legacyCipherSuites},
	{name: "tls12-cbc", version: tls.VersionTLS12, suites: []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	}},
	{name: "tls12-chacha", version: tls.VersionTLS12, suites: []uint16{
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	}},
	{name: "tls12-rsakx", version: tls.VersionTLS12, suites: []uint16{
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	}},
}

func probeTLS(host, port string, probe tlsHandshakeProbe) (string, string) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	config := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         probe.version,
		MaxVersion:         probe.version,
		CipherSuites:       probe.suites,
	}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, port), config)
	if err == nil {
		defer conn.Close()
		state := conn.ConnectionState()
		return "ACCEPTED", fmt.Sprintf("0x%04x %s", state.Version, tls.CipherSuiteName(state.CipherSuite))
	}
	message := err.Error()
	if strings.Contains(message, "remote error: tls:") {
		return "REFUSED", message
	}
	return "ERROR", message
}

func routeHost(ctx context.Context, namespace, name string) (string, error) {
	gvr := schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}
	obj, err := dynamicClient.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	host, _, _ := unstructured.NestedString(obj.Object, "spec", "host")
	return host, nil
}

var csvVersionPattern = regexp.MustCompile(`\.v?\d`)

func csvPackageName(name string) string {
	location := csvVersionPattern.FindStringIndex(name)
	if location == nil {
		return name
	}
	return name[:location[0]]
}

func podGPUResources(pod *corev1.Pod) map[string]string {
	out := map[string]string{}
	containers := append([]corev1.Container{}, pod.Spec.InitContainers...)
	containers = append(containers, pod.Spec.Containers...)
	for _, container := range containers {
		for _, list := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
			for name, quantity := range list {
				if strings.HasPrefix(string(name), "nvidia.com/") {
					out[string(name)] = quantity.String()
				}
			}
		}
	}
	return out
}

func gpuResourcesFromSpec(spec map[string]interface{}) map[string]string {
	out := map[string]string{}
	if spec == nil {
		return out
	}
	for _, field := range []string{"containers", "initContainers"} {
		containers, found, err := unstructured.NestedSlice(spec, field)
		if err != nil || !found {
			continue
		}
		for _, raw := range containers {
			container, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			for _, listName := range []string{"requests", "limits"} {
				resources, found, err := unstructured.NestedStringMap(container, "resources", listName)
				if err != nil || !found {
					continue
				}
				for name, quantity := range resources {
					if strings.HasPrefix(name, "nvidia.com/") {
						out[name] = quantity
					}
				}
			}
		}
	}
	return out
}

func workloadPodSpec(obj map[string]interface{}, kind string) map[string]interface{} {
	if kind == "CronJob" {
		spec, _, _ := unstructured.NestedMap(obj, "spec", "jobTemplate", "spec", "template", "spec")
		return spec
	}
	spec, _, _ := unstructured.NestedMap(obj, "spec", "template", "spec")
	return spec
}

func podGPUAllocation(pod *corev1.Pod) map[string]int64 {
	out := map[string]int64{}
	containers := append([]corev1.Container{}, pod.Spec.InitContainers...)
	containers = append(containers, pod.Spec.Containers...)
	for _, container := range containers {
		seen := map[string]bool{}
		for name, quantity := range container.Resources.Requests {
			key := string(name)
			if strings.HasPrefix(key, "nvidia.com/") {
				out[key] += quantity.Value()
				seen[key] = true
			}
		}
		for name, quantity := range container.Resources.Limits {
			key := string(name)
			if strings.HasPrefix(key, "nvidia.com/") && !seen[key] {
				out[key] += quantity.Value()
			}
		}
	}
	return out
}

func nodeHasGPU(node *corev1.Node) bool {
	if node.Labels["nvidia.com/gpu.present"] == "true" {
		return true
	}
	capacity, ok := node.Status.Capacity[corev1.ResourceName("nvidia.com/gpu")]
	return ok && capacity.Value() > 0
}

func gpuDeviceNames(spec map[string]interface{}) []string {
	if spec == nil {
		return nil
	}
	devices, _, _ := unstructured.NestedMap(spec, "domain", "devices")
	if devices == nil {
		return nil
	}
	var names []string
	for _, field := range []string{"gpus", "hostDevices"} {
		entries, _, _ := unstructured.NestedSlice(devices, field)
		for _, raw := range entries {
			entry, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			name, _, _ := unstructured.NestedString(entry, "deviceName")
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

func virtualMachineTestSpec(obj map[string]interface{}, kind string) map[string]interface{} {
	if kind == "VirtualMachine" {
		spec, _, _ := unstructured.NestedMap(obj, "spec", "template", "spec")
		return spec
	}
	spec, _, _ := unstructured.NestedMap(obj, "spec")
	return spec
}

func permittedHostDevices(spec map[string]interface{}) map[string]bool {
	out := map[string]bool{}
	if spec == nil {
		return out
	}
	var scopes []map[string]interface{}
	for _, fields := range [][]string{
		{"virtualization", "permittedHostDevices"},
		{"configuration", "permittedHostDevices"},
		{"permittedHostDevices"},
	} {
		if scope, _, _ := unstructured.NestedMap(spec, fields...); scope != nil {
			scopes = append(scopes, scope)
		}
	}
	for _, scope := range scopes {
		for _, field := range []string{"pciHostDevices", "mediatedDevices"} {
			entries, _, _ := unstructured.NestedSlice(scope, field)
			for _, raw := range entries {
				entry, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				name, _, _ := unstructured.NestedString(entry, "resourceName")
				if name == "" {
					continue
				}
				external, _, _ := unstructured.NestedBool(entry, "externalResourceProvider")
				out[name] = external
			}
		}
	}
	return out
}

func formatResourceMap(resources map[string]string) string {
	keys := make([]string, 0, len(resources))
	for key := range resources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+resources[key])
	}
	return strings.Join(parts, " ")
}

func formatInt64Map(values map[string]int64) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+strconv.FormatInt(values[key], 10))
	}
	return strings.Join(parts, " ")
}

func podsOnNode(ctx context.Context, namespace, node string) ([]corev1.Pod, error) {
	list, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node,
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func resourceNamesFromHostDevices(obj map[string]interface{}, fields ...string) []string {
	slice, found, err := unstructured.NestedSlice(obj, fields...)
	if err != nil || !found {
		return nil
	}
	var out []string
	for _, item := range slice {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(entry, "resourceName")
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func sarAllowed(ctx context.Context, verb, name string) (bool, error) {
	review, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx,
		&authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User:   "gpu-switch-probe",
				Groups: []string{"gpu-switchers", "system:authenticated"},
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Verb:     verb,
					Resource: "nodes",
					Name:     name,
				},
			},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		return false, err
	}
	return review.Status.Allowed, nil
}

var _ = Describe("Compliance admin", Label("pass-fail"), func() {
	It("T-01 FIPS mode on every node", func() {
		var failures []string
		for _, node := range nodeNames {
			if err, ok := probeFailure(node); ok {
				failures = append(failures, fmt.Sprintf("%s: %s", node, err))
				continue
			}
			v := values(probeLogs[node])
			if v["FIPS"] != "1" || v["CMDLINE_FIPS"] != "fips=1" || v["POLICY"] != "FIPS" {
				failures = append(failures, fmt.Sprintf("%s fips=%q cmdline=%q policy=%q",
					node, v["FIPS"], v["CMDLINE_FIPS"], v["POLICY"]))
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-19 local disks are LUKS2 with a Tang binding", func() {
		if isHostedCluster && platformType == "KubeVirt" {
			Skip("KubeVirt hosted cluster: its workers' disks are the hypervisor's")
		}
		var failures []string
		for _, node := range nodeNames {
			if err, ok := probeFailure(node); ok {
				failures = append(failures, fmt.Sprintf("%s: %s", node, err))
				continue
			}
			log := probeLogs[node]
			luks := linesWithPrefix(log, "LUKS=")
			plain := linesWithPrefix(log, "PLAIN=")
			if len(luks) == 0 {
				failures = append(failures, fmt.Sprintf("%s no LUKS devices", node))
			}
			for _, line := range luks {
				fields := strings.Fields(strings.TrimPrefix(line, "LUKS="))
				if len(fields) < 3 {
					failures = append(failures, fmt.Sprintf("%s malformed %q", node, line))
					continue
				}
				dev := fields[0]
				version := strings.TrimPrefix(fields[1], "v=")
				tang := strings.TrimPrefix(fields[2], "tang=")
				if version != "2" || tang == "" || tang == "0" {
					failures = append(failures, fmt.Sprintf("%s %s v=%s tang=%s", node, dev, version, tang))
				}
			}
			if len(plain) > 0 {
				failures = append(failures, fmt.Sprintf("%s unencrypted: %s", node, strings.Join(plain, ", ")))
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-21 clocks are synchronised", func() {
		var failures []string
		for _, node := range nodeNames {
			if err, ok := probeFailure(node); ok {
				failures = append(failures, fmt.Sprintf("%s: %s", node, err))
				continue
			}
			v := values(probeLogs[node])
			if v["CHRONYD"] != "active" || v["NTPSYNC"] != "yes" {
				failures = append(failures, fmt.Sprintf("%s chronyd=%s ntp=%s",
					node, v["CHRONYD"], v["NTPSYNC"]))
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-23 USB storage is blocked", func() {
		var failures []string
		for _, node := range nodeNames {
			if err, ok := probeFailure(node); ok {
				failures = append(failures, fmt.Sprintf("%s: %s", node, err))
				continue
			}
			v := values(probeLogs[node])
			if v["NOUSB"] != "1" && v["USBGUARD"] != "active" {
				failures = append(failures, fmt.Sprintf("%s nousb=%s usbguard=%s",
					node, v["NOUSB"], v["USBGUARD"]))
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-11 SCC capability exceptions are known", func() {
		gvr := schema.GroupVersionResource{
			Group: "security.openshift.io", Version: "v1", Resource: "securitycontextconstraints",
		}
		list, err := dynamicClient.Resource(gvr).List(context.Background(), metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())

		defaultSCC := regexp.MustCompile(`^(privileged|hostnetwork-v2|restricted-v2|restricted-v3|nonroot-v2|insights-runtime-extractor-scc|nested-container)$`)
		var offenders, unexpected []string
		for _, item := range list.Items {
			name := item.GetName()
			caps, found, err := unstructured.NestedStringSlice(item.Object, "allowedCapabilities")
			if err != nil || !found || len(caps) == 0 {
				continue
			}
			if defaultSCC.MatchString(name) {
				continue
			}
			offenders = append(offenders, name)
			matched := false
			for _, pattern := range allowedSCCPatterns {
				if pattern.MatchString(name) {
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			unexpected = append(unexpected, name)
		}
		fmt.Printf("SCCs with allowedCapabilities outside the default allowlist: %v\n", offenders)
		Expect(unexpected).To(BeEmpty(), "unexpected privileged SCCs: %s", strings.Join(unexpected, ", "))
	})

	It("T-20 OAuth token lifetime and idle timeout", func() {
		if isHostedCluster {
			Skip("hosted cluster: set on its HostedCluster (spec.configuration.oauth), check on the hub")
		}
		gvr := schema.GroupVersionResource{
			Group: "config.openshift.io", Version: "v1", Resource: "oauths",
		}
		obj, err := dynamicClient.Resource(gvr).Get(context.Background(), "cluster", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		age, found, err := unstructured.NestedInt64(obj.Object, "spec", "tokenConfig", "accessTokenMaxAgeSeconds")
		Expect(err).NotTo(HaveOccurred())
		idle, foundIdle, err := unstructured.NestedString(obj.Object, "spec", "tokenConfig", "accessTokenInactivityTimeout")
		Expect(err).NotTo(HaveOccurred())

		var failures []string
		if !found || age <= 0 || age > 86400 {
			failures = append(failures, fmt.Sprintf("accessTokenMaxAgeSeconds=%d (want <= 86400)", age))
		}
		if !foundIdle || idle == "" {
			failures = append(failures, "accessTokenInactivityTimeout unset")
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-13 NFS storage uses TLS", func() {
		ctx := context.Background()
		var failures []string

		ds, err := clientset.AppsV1().DaemonSets("nfs-tls").Get(ctx, "tlshd", metav1.GetOptions{})
		if err != nil {
			fmt.Printf("WARN tlshd DaemonSet unavailable in nfs-tls: %v; NFS over TLS is not deployed\n", err)
			return
		}
		if ds.Status.DesiredNumberScheduled == 0 || ds.Status.NumberReady != ds.Status.DesiredNumberScheduled {
			failures = append(failures, fmt.Sprintf("tlshd not Ready: %d/%d", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
		} else {
			fmt.Printf("PASS tlshd Ready on %d/%d node(s)\n", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
		}

		scs, err := clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		nfs := 0
		for _, sc := range scs.Items {
			mountOptions := strings.Join(sc.MountOptions, " ")
			backend := ""
			if sc.Parameters != nil {
				backend = sc.Parameters["backend"]
			}
			if !strings.Contains(mountOptions, "nfsvers") && backend != "pure_file" {
				continue
			}
			nfs++
			if !strings.Contains(mountOptions, "xprtsec=tls") {
				failures = append(failures, fmt.Sprintf("StorageClass %s is NFS without xprtsec=tls (mountOptions: %s)", sc.Name, mountOptions))
			}
		}
		if nfs == 0 {
			failures = append(failures, "no NFS StorageClass found")
		}

		totalTLS, totalClear := 0, 0
		for _, node := range nodeNames {
			if err, ok := probeFailure(node); ok {
				failures = append(failures, fmt.Sprintf("%s: %s", node, err))
				continue
			}
			v := values(probeLogs[node])
			tls, _ := strconv.Atoi(v["NFS_TLS"])
			clear, _ := strconv.Atoi(v["NFS_CLEAR"])
			totalTLS += tls
			totalClear += clear
		}
		fmt.Printf("NFS mounts: xprtsec=tls %d, cleartext %d\n", totalTLS, totalClear)
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-03 compliance scans are fresh and not regressed", func() {
		ctx := context.Background()
		coNS := "openshift-compliance"
		suiteGVR := schema.GroupVersionResource{Group: "compliance.openshift.io", Version: "v1alpha1", Resource: "compliancesuites"}
		scanGVR := schema.GroupVersionResource{Group: "compliance.openshift.io", Version: "v1alpha1", Resource: "compliancescans"}
		ccrGVR := schema.GroupVersionResource{Group: "compliance.openshift.io", Version: "v1alpha1", Resource: "compliancecheckresults"}

		var failures, warnings []string
		suites, err := dynamicClient.Resource(suiteGVR).Namespace(coNS).List(ctx, metav1.ListOptions{})
		switch {
		case isMissing(err):
			failures = append(failures, "no ComplianceSuite API: Compliance Operator is not installed")
		case err != nil:
			failures = append(failures, fmt.Sprintf("list ComplianceSuites: %v", err))
		case len(suites.Items) == 0:
			failures = append(failures, "no ComplianceSuites found")
		default:
			for _, suite := range suites.Items {
				phase, _, _ := unstructured.NestedString(suite.Object, "status", "phase")
				if phase != "DONE" {
					failures = append(failures, fmt.Sprintf("suite %s phase=%q (want DONE)", suite.GetName(), phase))
				} else {
					fmt.Printf("PASS suite %s phase=DONE\n", suite.GetName())
				}
			}
		}

		scans, err := dynamicClient.Resource(scanGVR).Namespace(coNS).List(ctx, metav1.ListOptions{})
		if err != nil && !isMissing(err) {
			failures = append(failures, fmt.Sprintf("list ComplianceScans: %v", err))
		}
		if scans == nil {
			scans = &unstructured.UnstructuredList{}
		}
		var newest time.Time
		for _, scan := range scans.Items {
			end, found, _ := unstructured.NestedString(scan.Object, "status", "endTimestamp")
			if !found || end == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339, end)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("scan %s has unparseable endTimestamp %q", scan.GetName(), end))
				continue
			}
			if t.After(newest) {
				newest = t
			}
		}
		if newest.IsZero() {
			if len(scans.Items) > 0 {
				failures = append(failures, "no ComplianceScan status.endTimestamp: cannot verify scan freshness")
			}
		} else {
			age := hoursSince(newest)
			if age <= scanMaxAgeHours {
				fmt.Printf("PASS newest scan %dh old (<= %dh)\n", age, scanMaxAgeHours)
			} else {
				warnings = append(warnings, fmt.Sprintf("newest scan %dh old (> %dh): scans may have stopped", age, scanMaxAgeHours))
			}

			mcGVR := schema.GroupVersionResource{Group: "machineconfiguration.openshift.io", Version: "v1", Resource: "machineconfigs"}
			mcs, err := dynamicClient.Resource(mcGVR).List(ctx, metav1.ListOptions{})
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("list MachineConfigs: %v", err))
			} else {
				var newestMC time.Time
				for _, mc := range mcs.Items {
					if t := mc.GetCreationTimestamp().Time; t.After(newestMC) {
						newestMC = t
					}
				}
				if !newestMC.IsZero() && newestMC.After(newest) {
					warnings = append(warnings, fmt.Sprintf(
						"STALE: newest MachineConfig (%s) is newer than the newest scan (%s); results predate the last cluster change",
						newestMC.UTC().Format(time.RFC3339), newest.UTC().Format(time.RFC3339)))
				}
			}
		}

		for _, scan := range scans.Items {
			selector := fmt.Sprintf("compliance.openshift.io/scan-name=%s,compliance.openshift.io/check-status=FAIL", scan.GetName())
			results, err := dynamicClient.Resource(ccrGVR).Namespace(coNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				failures = append(failures, fmt.Sprintf("list FAIL results for %s: %v", scan.GetName(), err))
				continue
			}
			n := len(results.Items)
			fmt.Printf("scan %s: %d FAIL\n", scan.GetName(), n)
			base, configured := scanBaseline[scan.GetName()]
			if !configured {
				warnings = append(warnings, fmt.Sprintf("%s: %d FAIL (no baseline configured)", scan.GetName(), n))
				continue
			}
			switch {
			case n > base:
				failures = append(failures, fmt.Sprintf("%s: %d FAIL (baseline %d) REGRESSED", scan.GetName(), n, base))
			case n < base:
				fmt.Printf("PASS %s: %d FAIL (baseline %d) improved\n", scan.GetName(), n, base)
			default:
				fmt.Printf("PASS %s: %d FAIL (matches baseline)\n", scan.GetName(), n)
			}
		}

		if isHostedCluster {
			fmt.Println("SKIP openshift-apiserver audit-log-path: hosted control plane runs on the hub")
		} else {
			cm, err := clientset.CoreV1().ConfigMaps("openshift-apiserver").Get(ctx, "config", metav1.GetOptions{})
			if err != nil {
				failures = append(failures, fmt.Sprintf("read openshift-apiserver ConfigMap config: %v", err))
			} else {
				var cfg struct {
					APIServerArguments map[string][]string `json:"apiServerArguments"`
				}
				if err := json.Unmarshal([]byte(cm.Data["config.yaml"]), &cfg); err != nil {
					failures = append(failures, fmt.Sprintf("parse openshift-apiserver config: %v", err))
				} else {
					path := ""
					if values := cfg.APIServerArguments["audit-log-path"]; len(values) > 0 {
						path = values[0]
					}
					if path != "/var/log/openshift-apiserver/audit.log" {
						failures = append(failures, fmt.Sprintf("openshift-apiserver audit-log-path is %q, expected /var/log/openshift-apiserver/audit.log", path))
					} else {
						fmt.Printf("PASS openshift-apiserver audit-log-path=%s\n", path)
					}
				}
			}
		}

		for _, warning := range warnings {
			fmt.Printf("WARN %s\n", warning)
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-04 raw ARF evidence is archived", func() {
		ctx := context.Background()
		coNS := "openshift-compliance"
		ssGVR := schema.GroupVersionResource{Group: "compliance.openshift.io", Version: "v1alpha1", Resource: "scansettings"}

		var failures, warnings []string
		settings, err := dynamicClient.Resource(ssGVR).Namespace(coNS).List(ctx, metav1.ListOptions{})
		if err != nil && !isMissing(err) {
			failures = append(failures, fmt.Sprintf("list ScanSettings: %v", err))
		}
		if settings == nil {
			settings = &unstructured.UnstructuredList{}
		}
		for _, setting := range settings.Items {
			enabled, found, _ := unstructured.NestedBool(setting.Object, "rawResultStorage", "enabled")
			if found && !enabled {
				warnings = append(warnings, fmt.Sprintf("ScanSetting %s has rawResultStorage.enabled=false: results are not durable audit evidence", setting.GetName()))
			}
		}

		pvcs, err := clientset.CoreV1().PersistentVolumeClaims(coNS).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				failures = append(failures, fmt.Sprintf("namespace %s does not exist: no result PVCs", coNS))
			} else {
				failures = append(failures, fmt.Sprintf("list PVCs in %s: %v", coNS, err))
			}
		} else {
			bound := 0
			for _, pvc := range pvcs.Items {
				if pvc.Status.Phase == corev1.ClaimBound {
					bound++
				}
			}
			if bound > 0 {
				fmt.Printf("PASS %d bound result PVC(s) in %s\n", bound, coNS)
			} else {
				failures = append(failures, fmt.Sprintf("no bound result PVCs in %s: raw ARF is not archived", coNS))
			}
		}

		for _, warning := range warnings {
			fmt.Printf("WARN %s\n", warning)
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-05 RHACS is healthy", func() {
		ctx := context.Background()
		acsNS := "stackrox"
		centralGVR := schema.GroupVersionResource{Group: "platform.stackrox.io", Version: "v1alpha1", Resource: "centrals"}
		securedGVR := schema.GroupVersionResource{Group: "platform.stackrox.io", Version: "v1alpha1", Resource: "securedclusters"}

		var failures []string
		if isHostedCluster {
			fmt.Println("SKIP Central Available: hosted cluster, Central runs on the hub")
		} else {
			central, err := dynamicClient.Resource(centralGVR).Namespace(acsNS).Get(ctx, "stackrox-central-services", metav1.GetOptions{})
			switch {
			case isMissing(err):
				failures = append(failures, "Central: API not installed")
			case err != nil:
				failures = append(failures, fmt.Sprintf("read Central: %v", err))
			case conditionStatus(central.Object, "Available") != "True":
				failures = append(failures, fmt.Sprintf("Central Available=%q", conditionStatus(central.Object, "Available")))
			default:
				fmt.Println("PASS Central Available")
			}
		}

		secured, err := dynamicClient.Resource(securedGVR).Namespace(acsNS).Get(ctx, "stackrox-secured-cluster-services", metav1.GetOptions{})
		switch {
		case isMissing(err):
			failures = append(failures, "SecuredCluster: API not installed")
		case err != nil:
			failures = append(failures, fmt.Sprintf("read SecuredCluster: %v", err))
		case conditionStatus(secured.Object, "Available") != "True":
			failures = append(failures, fmt.Sprintf("SecuredCluster Available=%q", conditionStatus(secured.Object, "Available")))
		default:
			fmt.Println("PASS SecuredCluster Available")
		}

		pods, err := clientset.CoreV1().Pods(acsNS).List(ctx, metav1.ListOptions{})
		switch {
		case apierrors.IsNotFound(err):
			fmt.Printf("INFO namespace %s does not exist\n", acsNS)
		case err != nil:
			failures = append(failures, fmt.Sprintf("list pods in %s: %v", acsNS, err))
		case len(pods.Items) == 0:
			failures = append(failures, fmt.Sprintf("no pods in %s", acsNS))
		default:
			notReady, collectors := 0, 0
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.Status.Phase == corev1.PodSucceeded {
					continue
				}
				if strings.HasPrefix(pod.Name, "collector-") {
					collectors++
				}
				if pod.Status.Phase != corev1.PodRunning || !podReady(pod) {
					notReady++
				}
			}
			if notReady == 0 {
				fmt.Printf("PASS all %s pods ready\n", acsNS)
			} else {
				failures = append(failures, fmt.Sprintf("%d pod(s) not ready in %s", notReady, acsNS))
			}
			if collectors == len(nodeNames) {
				fmt.Printf("PASS collector on all %d nodes\n", len(nodeNames))
			} else {
				fmt.Printf("WARN collector on %d of %d nodes\n", collectors, len(nodeNames))
			}
		}

		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-14 etcd backups are running", func() {
		if isHostedCluster {
			Skip("hosted cluster: etcd runs on the hub; backups are checked there")
		}
		ctx := context.Background()
		ns := "etcd-backup"
		var failures []string

		cron, err := clientset.BatchV1().CronJobs(ns).Get(ctx, "etcd-backup", metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				failures = append(failures, fmt.Sprintf("no etcd-backup CronJob in %s: etcd is not being backed up", ns))
			} else {
				failures = append(failures, fmt.Sprintf("read etcd-backup CronJob: %v", err))
			}
			Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
			return
		}
		if cron.Spec.Suspend != nil && *cron.Spec.Suspend {
			fmt.Println("WARN CronJob etcd-backup is SUSPENDED")
		} else {
			fmt.Printf("PASS CronJob etcd-backup active (%s)\n", cron.Spec.Schedule)
		}

		jobs, err := clientset.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			failures = append(failures, fmt.Sprintf("list backup jobs: %v", err))
			Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
			return
		}
		var newest time.Time
		for _, job := range jobs.Items {
			if job.Status.Succeeded > 0 && job.Status.CompletionTime != nil && job.Status.CompletionTime.Time.After(newest) {
				newest = job.Status.CompletionTime.Time
			}
		}
		if newest.IsZero() {
			failures = append(failures, "no successful backup job on record")
			Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
			return
		}
		age := hoursSince(newest)
		if age <= backupMaxAgeHours {
			fmt.Printf("PASS newest successful backup %dh old (<= %dh)\n", age, backupMaxAgeHours)
		} else {
			failures = append(failures, fmt.Sprintf("newest successful backup %dh old (> %dh): backups have stopped", age, backupMaxAgeHours))
		}

		failed := 0
		for _, job := range jobs.Items {
			if job.Status.Failed > 0 {
				failed++
			}
		}
		if failed > 0 {
			fmt.Printf("WARN %d failed backup job(s) retained\n", failed)
		}

		if containers := cron.Spec.JobTemplate.Spec.Template.Spec.Containers; len(containers) > 0 && containers[0].Image != "" {
			image := containers[0].Image
			if cliImage == "" {
				fmt.Printf("INFO backup job image %s (set UAT_CLI_IMAGE to compare)\n", image)
			} else if image != cliImage {
				fmt.Printf("WARN backup job image is not the configured cli image (%s vs %s)\n", image, cliImage)
			} else {
				fmt.Println("PASS backup job image is the configured cli image")
			}
		}

		pvc, err := clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "etcd-backup", metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			failures = append(failures, fmt.Sprintf("no etcd-backup PVC in %s", ns))
		case err != nil:
			failures = append(failures, fmt.Sprintf("read etcd-backup PVC: %v", err))
		case pvc.Spec.VolumeName == "":
			failures = append(failures, "etcd-backup PVC is not bound")
		default:
			pv, err := clientset.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
			switch {
			case err != nil:
				failures = append(failures, fmt.Sprintf("read backup PV %s: %v", pvc.Spec.VolumeName, err))
			case pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain:
				failures = append(failures, fmt.Sprintf("backup PV reclaim policy is %q, want Retain", pv.Spec.PersistentVolumeReclaimPolicy))
			default:
				fmt.Println("PASS backup PV reclaim policy Retain")
			}
		}

		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-22 API audit profile and forwarding", func() {
		ctx := context.Background()
		loggingNS := "openshift-logging"
		var failures []string

		apiGVR := schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "apiservers"}
		api, err := dynamicClient.Resource(apiGVR).Get(ctx, "cluster", metav1.GetOptions{})
		switch {
		case isMissing(err):
			failures = append(failures, "APIServer config API not installed")
		case err != nil:
			failures = append(failures, fmt.Sprintf("read APIServer config: %v", err))
		default:
			profile, _, _ := unstructured.NestedString(api.Object, "spec", "audit", "profile")
			switch profile {
			case "WriteRequestBodies", "AllRequestBodies":
				fmt.Printf("PASS API audit profile %s\n", profile)
			default:
				failures = append(failures, fmt.Sprintf("API audit profile %q (want WriteRequestBodies)", profile))
			}
		}

		if _, err := clientset.CoreV1().Namespaces().Get(ctx, loggingNS, metav1.GetOptions{}); err != nil {
			if apierrors.IsNotFound(err) {
				failures = append(failures, fmt.Sprintf("no %s: audit logs are not forwarded", loggingNS))
			} else {
				failures = append(failures, fmt.Sprintf("read namespace %s: %v", loggingNS, err))
			}
			Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
			return
		}

		clfGVR := schema.GroupVersionResource{Group: "logging.openshift.io", Version: "v1", Resource: "clusterlogforwarders"}
		clfs, err := dynamicClient.Resource(clfGVR).Namespace(loggingNS).List(ctx, metav1.ListOptions{})
		if err != nil {
			if isMissing(err) {
				failures = append(failures, "no ClusterLogForwarder API: audit logs are not forwarded")
			} else {
				failures = append(failures, fmt.Sprintf("list ClusterLogForwarders: %v", err))
			}
			Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
			return
		}
		var clf *unstructured.Unstructured
		for i := range clfs.Items {
			if hasString(pipelineInputRefs(clfs.Items[i].Object), "audit") {
				clf = &clfs.Items[i]
				break
			}
		}
		if clf == nil {
			failures = append(failures, "no ClusterLogForwarder with the audit input")
			Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
			return
		}
		if ready := conditionStatus(clf.Object, "Ready"); ready != "True" {
			failures = append(failures, fmt.Sprintf("ClusterLogForwarder %s Ready=%q", clf.GetName(), ready))
		} else {
			fmt.Printf("PASS ClusterLogForwarder %s forwards audit, Ready\n", clf.GetName())
		}

		lokiGVR := schema.GroupVersionResource{Group: "loki.grafana.com", Version: "v1", Resource: "lokistacks"}
		lokis, err := dynamicClient.Resource(lokiGVR).Namespace(loggingNS).List(ctx, metav1.ListOptions{})
		switch {
		case isMissing(err):
			fmt.Println("INFO no LokiStack API: forwarded to a remote Loki, not checked")
		case err != nil:
			fmt.Printf("WARN list LokiStacks: %v\n", err)
		case len(lokis.Items) == 0:
			fmt.Println("INFO no LokiStack here: forwarded to a remote Loki, not checked")
		default:
			pods, err := clientset.CoreV1().Pods(loggingNS).List(ctx, metav1.ListOptions{})
			if err != nil {
				fmt.Printf("WARN list logging pods: %v\n", err)
				break
			}
			rate, size := 0, 0
			forbidden := false
			since := int64(600)
			for _, pod := range pods.Items {
				if !strings.HasPrefix(pod.Name, "loki-distributor") {
					continue
				}
				logs, err := clientset.CoreV1().Pods(loggingNS).GetLogs(pod.Name, &corev1.PodLogOptions{SinceSeconds: &since}).DoRaw(ctx)
				if apierrors.IsForbidden(err) {
					forbidden = true
					break
				}
				if err != nil {
					fmt.Printf("WARN read logs for %s: %v\n", pod.Name, err)
					continue
				}
				for _, line := range strings.Split(string(logs), "\n") {
					if !strings.Contains(line, "write operation failed") {
						continue
					}
					if strings.Contains(line, "rate limit exceeded") {
						rate++
					}
					if strings.Contains(line, "max entry size") {
						size++
					}
				}
			}
			if forbidden {
				fmt.Println("SKIP Loki rate/size check: pods/log is not permitted in openshift-logging (add a namespace RoleBinding to enable it)")
			} else {
				if size == 0 {
					fmt.Println("PASS no audit lines dropped for size in the last 10 minutes")
				} else {
					failures = append(failures, fmt.Sprintf("%d audit line(s) over Loki's line size limit in the last 10 minutes: dropped", size))
				}
				if rate == 0 {
					fmt.Println("PASS Loki under its rate limit in the last 10 minutes")
				} else {
					fmt.Printf("WARN Loki over its rate limit %d time(s) in the last 10 minutes; the collector retries\n", rate)
				}
			}
		}

		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-15 effective TLS is 1.2+ with AEAD only", func() {
		ctx := context.Background()
		var targets []tlsTarget
		addTarget := func(name, host, port string) {
			if host == "" {
				return
			}
			if port == "" {
				port = "443"
			}
			targets = append(targets, tlsTarget{name: name, host: host, port: port})
		}
		if parsed, err := url.Parse(apiServerHost); err == nil && parsed.Hostname() != "" {
			port := parsed.Port()
			if port == "" {
				port = "443"
			}
			addTarget("api-server", parsed.Hostname(), port)
		}
		if host, err := routeHost(ctx, "openshift-console", "console"); err == nil && host != "" {
			addTarget("ingress(console)", host, "443")
		} else if err != nil && !isMissing(err) {
			fmt.Printf("WARN read console route: %v\n", err)
		}
		if host, err := routeHost(ctx, "openshift-authentication", "oauth-openshift"); err == nil && host != "" {
			addTarget("oauth", host, "443")
		} else if err != nil && !isMissing(err) {
			fmt.Printf("WARN read oauth route: %v\n", err)
		}
		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, node := range nodes.Items {
			for _, address := range node.Status.Addresses {
				if address.Type == corev1.NodeInternalIP {
					addTarget("kubelet/"+node.Name, address.Address, "10250")
					break
				}
			}
		}
		Expect(targets).NotTo(BeEmpty(), "no TLS targets found")

		var failures []string
		for _, target := range targets {
			var fails, errs []string
			for _, probe := range tlsHandshakeProbes {
				result, detail := probeTLS(target.host, target.port, probe)
				switch {
				case probe.required && result != "ACCEPTED":
					fails = append(fails, fmt.Sprintf("%s(%s: %s)", probe.name, result, detail))
				case !probe.required && result == "ACCEPTED":
					fails = append(fails, fmt.Sprintf("%s(ACCEPTED: %s)", probe.name, detail))
				case !probe.required && result != "REFUSED":
					errs = append(errs, probe.name)
				}
			}
			switch {
			case len(fails) > 0:
				failures = append(failures, fmt.Sprintf("%s: %s", target.name, strings.Join(fails, " ")))
			case len(errs) > 0:
				fmt.Printf("WARN %s: could not probe %s\n", target.name, strings.Join(errs, ", "))
			default:
				fmt.Printf("PASS %s: TLS 1.3 + 1.2 AES-GCM only\n", target.name)
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-17 no GPU claimed or requested", func() {
		ctx := context.Background()
		var failures []string
		level := func(namespace string) string {
			if gpuClaimAllowRe != nil && gpuClaimAllowRe.MatchString(namespace) {
				return "WARN"
			}
			return "FAIL"
		}
		report := func(level, what, detail string) {
			if level == "FAIL" {
				failures = append(failures, fmt.Sprintf("%s: %s", what, detail))
			} else {
				fmt.Printf("WARN %s: %s\n", what, detail)
			}
		}

		pods, podsErr := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if podsErr != nil {
			failures = append(failures, fmt.Sprintf("list pods: %v", podsErr))
		} else {
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
					continue
				}
				resources := podGPUResources(pod)
				if len(resources) == 0 {
					continue
				}
				claimLevel := level(pod.Namespace)
				if pod.Namespace == "nvidia-gpu-operator" && strings.Contains(pod.Name, "cuda-validator") {
					claimLevel = "WARN"
				}
				report(claimLevel, fmt.Sprintf("pod %s/%s", pod.Namespace, pod.Name),
					fmt.Sprintf("phase=%s node=%s %s", pod.Status.Phase, pod.Spec.NodeName, formatResourceMap(resources)))
			}
		}

		workloads := []struct {
			gvr  schema.GroupVersionResource
			kind string
		}{
			{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "Deployment"},
			{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, "StatefulSet"},
			{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, "DaemonSet"},
			{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}, "ReplicaSet"},
			{schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, "Job"},
			{schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, "CronJob"},
		}
		for _, workload := range workloads {
			list, err := dynamicClient.Resource(workload.gvr).List(ctx, metav1.ListOptions{})
			if isMissing(err) {
				continue
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("list %s: %v", workload.kind, err))
				continue
			}
			if list == nil {
				continue
			}
			for _, item := range list.Items {
				if workload.kind == "ReplicaSet" || workload.kind == "Job" {
					owners, _, _ := unstructured.NestedSlice(item.Object, "metadata", "ownerReferences")
					if len(owners) > 0 {
						continue
					}
				}
				resources := gpuResourcesFromSpec(workloadPodSpec(item.Object, workload.kind))
				if len(resources) == 0 {
					continue
				}
				detail := "template requests " + formatResourceMap(resources)
				if replicas, found, _ := unstructured.NestedInt64(item.Object, "spec", "replicas"); found {
					detail += fmt.Sprintf(" replicas=%d", replicas)
				}
				report(level(item.GetNamespace()), fmt.Sprintf("%s %s/%s", workload.kind, item.GetNamespace(), item.GetName()), detail)
			}
		}

		vms := []struct {
			gvr  schema.GroupVersionResource
			kind string
		}{
			{schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}, "VirtualMachine"},
			{schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}, "VirtualMachineInstance"},
		}
		for _, vm := range vms {
			list, err := dynamicClient.Resource(vm.gvr).List(ctx, metav1.ListOptions{})
			if isMissing(err) {
				continue
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("list %s: %v", vm.kind, err))
				continue
			}
			if list == nil {
				continue
			}
			for _, item := range list.Items {
				var domain map[string]interface{}
				if vm.kind == "VirtualMachine" {
					domain, _, _ = unstructured.NestedMap(item.Object, "spec", "template", "spec", "domain")
				} else {
					domain, _, _ = unstructured.NestedMap(item.Object, "spec", "domain")
				}
				if domain == nil {
					continue
				}
				devices, _, _ := unstructured.NestedMap(domain, "devices")
				if devices == nil {
					continue
				}
				var names []string
				for _, field := range []string{"gpus", "hostDevices"} {
					entries, _, _ := unstructured.NestedSlice(devices, field)
					for _, raw := range entries {
						entry, ok := raw.(map[string]interface{})
						if !ok {
							continue
						}
						name, _, _ := unstructured.NestedString(entry, "deviceName")
						if name != "" {
							names = append(names, name)
						}
					}
				}
				if len(names) == 0 {
					continue
				}
				state := ""
				if vm.kind == "VirtualMachine" {
					if strategy, found, _ := unstructured.NestedString(item.Object, "spec", "runStrategy"); found {
						state = strategy
					} else if running, found, _ := unstructured.NestedBool(item.Object, "spec", "running"); found {
						state = fmt.Sprintf("running=%t", running)
					}
				} else {
					state, _, _ = unstructured.NestedString(item.Object, "status", "phase")
				}
				report(level(item.GetNamespace()), fmt.Sprintf("%s %s/%s", vm.kind, item.GetNamespace(), item.GetName()),
					fmt.Sprintf("devices=%v state=%s", names, state))
			}
		}

		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			failures = append(failures, fmt.Sprintf("list nodes: %v", err))
		} else {
			allocations := map[string]map[string]int64{}
			if pods != nil {
				for i := range pods.Items {
					pod := &pods.Items[i]
					if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || pod.Spec.NodeName == "" {
						continue
					}
					allocation := podGPUAllocation(pod)
					if len(allocation) == 0 {
						continue
					}
					if allocations[pod.Spec.NodeName] == nil {
						allocations[pod.Spec.NodeName] = map[string]int64{}
					}
					for name, value := range allocation {
						allocations[pod.Spec.NodeName][name] += value
					}
				}
			}
			for _, node := range nodes.Items {
				if !nodeHasGPU(&node) {
					continue
				}
				if allocation := allocations[node.Name]; len(allocation) > 0 {
					failures = append(failures, fmt.Sprintf("%s: GPUs allocated: %s", node.Name, formatInt64Map(allocation)))
				} else {
					fmt.Printf("PASS %s: 0 GPUs allocated\n", node.Name)
				}
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-18 installed operators claim FIPS support", func() {
		ctx := context.Background()
		gvr := schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "clusterserviceversions"}
		list, err := dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred(), "list ClusterServiceVersions")

		var failures []string
		okCount := 0
		if list != nil {
			for _, item := range list.Items {
				if copied, _, _ := unstructured.NestedString(item.Object, "metadata", "labels", "olm.copiedFrom"); copied != "" {
					continue
				}
				name := item.GetName()
				if name == "packageserver" {
					continue
				}
				flag, found, _ := unstructured.NestedString(item.Object, "metadata", "annotations", "features.operators.openshift.io/fips-compliant")
				if !found {
					flag = "none"
				}
				if flag == "true" {
					okCount++
					continue
				}
				pkg := csvPackageName(name)
				if reason, accepted := operatorFipsAccepted[pkg]; accepted {
					fmt.Printf("WARN %s: flag %s; accepted: %s\n", name, flag, reason)
					continue
				}
				failures = append(failures, fmt.Sprintf("%s: flag %s; not in the accepted list", name, flag))
			}
		}
		fmt.Printf("PASS %d operators flagged fips-compliant=true\n", okCount)
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-02 FIPS validation record", func() {
		ctx := context.Background()
		var warnings []string
		if fipsRhcos == "" && fipsProviderVersion == "" {
			fmt.Println("WARN no FIPS validation record configured: set uat_compliance.fipsRhcos and fipsProviderVersion from the CMVP record")
			return
		}
		if fipsRhcos != "" {
			nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			for _, node := range nodes.Items {
				if node.Status.NodeInfo.OSImage != fipsRhcos {
					warnings = append(warnings, fmt.Sprintf("%s RHCOS=%q want %q", node.Name, node.Status.NodeInfo.OSImage, fipsRhcos))
				}
			}
		}
		if fipsProviderVersion != "" {
			for _, node := range nodeNames {
				if err, ok := probeFailure(node); ok {
					warnings = append(warnings, fmt.Sprintf("%s: %s", node, err))
					continue
				}
				provider := values(probeLogs[node])["FIPS_PROVIDER"]
				if provider != fipsProviderVersion {
					warnings = append(warnings, fmt.Sprintf("%s FIPS provider=%q want %q", node, provider, fipsProviderVersion))
				}
			}
		}
		for _, warning := range warnings {
			fmt.Printf("WARN %s\n", warning)
		}
		// T-02 reviews the validation record. verify.sh warns on a missing or
		// stale record; T-01 is the hard check that FIPS mode is on.
	})

	It("T-06 GPU container modality", func() {
		ctx := context.Background()
		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		gpu := corev1.ResourceName("nvidia.com/gpu")
		var failures []string
		found := false
		for _, node := range nodes.Items {
			capacity := node.Status.Capacity[gpu]
			if capacity.IsZero() {
				continue
			}
			mode := node.Labels["nvidia.com/gpu.workload.config"]
			if mode != "" && mode != "container" {
				continue
			}
			found = true
			alloc := node.Status.Allocatable[gpu]
			if alloc.IsZero() {
				failures = append(failures, fmt.Sprintf("%s container mode but nvidia.com/gpu=%s", node.Name, alloc.String()))
			}
			pods, err := podsOnNode(ctx, "nvidia-gpu-operator", node.Name)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s list gpu-operator pods: %v", node.Name, err))
				continue
			}
			running := false
			for _, pod := range pods {
				if strings.HasPrefix(pod.Name, "nvidia-driver-daemonset") && pod.Status.Phase == corev1.PodRunning {
					running = true
					break
				}
			}
			if !running {
				failures = append(failures, fmt.Sprintf("%s container mode but no driver daemonset Running", node.Name))
			}
		}
		if !found {
			Skip("no node currently in container modality")
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-07 GPU VM-passthrough modality", func() {
		ctx := context.Background()
		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		var failures []string
		seen := map[string]bool{}
		found := false
		for _, node := range nodes.Items {
			if node.Labels["nvidia.com/gpu.workload.config"] != "vm-passthrough" {
				continue
			}
			found = true
			advertised := map[string]string{}
			for name, qty := range node.Status.Allocatable {
				resource := string(name)
				if !strings.HasPrefix(resource, "nvidia.com/") || resource == "nvidia.com/gpu" {
					continue
				}
				if deviceResource != "" && resource != deviceResource {
					continue
				}
				advertised[resource] = qty.String()
			}
			if len(advertised) == 0 {
				suffix := ""
				if deviceResource != "" {
					suffix = " " + deviceResource
				}
				failures = append(failures, fmt.Sprintf("%s in vm-passthrough but advertises no device resource%s", node.Name, suffix))
			}
			for resource, value := range advertised {
				seen[resource] = true
				if value == "0" {
					failures = append(failures, fmt.Sprintf("%s in vm-passthrough but advertises %s=%s", node.Name, resource, value))
				}
			}
			pods, err := podsOnNode(ctx, "nvidia-gpu-operator", node.Name)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s list gpu-operator pods: %v", node.Name, err))
				continue
			}
			vfio, driver := false, false
			for _, pod := range pods {
				if strings.HasPrefix(pod.Name, "nvidia-vfio-manager") && pod.Status.Phase == corev1.PodRunning {
					vfio = true
				}
				if strings.HasPrefix(pod.Name, "nvidia-driver-daemonset") {
					driver = true
				}
			}
			if !vfio {
				failures = append(failures, fmt.Sprintf("%s vm-passthrough but no vfio-manager Running", node.Name))
			}
			if driver {
				failures = append(failures, fmt.Sprintf("%s host driver still present in vm-passthrough mode", node.Name))
			}
		}
		if !found {
			Skip("no node currently in vm-passthrough modality")
		}
		hcoGVR := schema.GroupVersionResource{Group: "hco.kubevirt.io", Version: "v1", Resource: "hyperconvergeds"}
		kvGVR := schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}
		hco, err := dynamicClient.Resource(hcoGVR).Namespace("openshift-cnv").Get(ctx, "kubevirt-hyperconverged", metav1.GetOptions{})
		if err != nil {
			failures = append(failures, fmt.Sprintf("read HyperConverged: %v", err))
		} else {
			permitted := resourceNamesFromHostDevices(hco.Object, "spec", "virtualization", "permittedHostDevices", "pciHostDevices")
			if len(permitted) == 0 {
				permitted = resourceNamesFromHostDevices(hco.Object, "spec", "permittedHostDevices", "pciHostDevices")
			}
			for resource := range seen {
				if !hasString(permitted, resource) {
					failures = append(failures, fmt.Sprintf("HCO does NOT permit %s", resource))
				}
			}
		}
		kv, err := dynamicClient.Resource(kvGVR).Namespace("openshift-cnv").Get(ctx, "kubevirt-kubevirt-hyperconverged", metav1.GetOptions{})
		if err != nil {
			failures = append(failures, fmt.Sprintf("read KubeVirt: %v", err))
		} else {
			permitted := resourceNamesFromHostDevices(kv.Object, "spec", "configuration", "permittedHostDevices", "pciHostDevices")
			for resource := range seen {
				if !hasString(permitted, resource) {
					failures = append(failures, fmt.Sprintf("KubeVirt does NOT permit %s", resource))
				}
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-10 registry allowlist", func() {
		ctx := context.Background()
		gvr := schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "images"}
		obj, err := dynamicClient.Resource(gvr).Get(ctx, "cluster", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		allowed, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "registrySources", "allowedRegistries")
		Expect(err).NotTo(HaveOccurred())
		if !found || len(allowed) == 0 {
			Skip("allowedRegistries is unset")
		}
		pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		images := map[string]bool{}
		for _, pod := range pods.Items {
			for _, container := range pod.Spec.Containers {
				images[container.Image] = true
			}
			for _, container := range pod.Spec.InitContainers {
				images[container.Image] = true
			}
		}
		var bad []string
		for image := range images {
			ref := image
			parts := strings.Split(ref, "/")
			if len(parts) == 2 && parts[0] == "docker.io" {
				ref = "docker.io/library/" + parts[1]
			}
			covered := false
			for _, entry := range allowed {
				if ref == entry || strings.HasPrefix(ref, entry+"/") || strings.HasPrefix(ref, entry+":") || strings.HasPrefix(ref, entry+"@") {
					covered = true
					break
				}
			}
			if !covered {
				bad = append(bad, image)
			}
		}
		sort.Strings(bad)
		Expect(bad).To(BeEmpty(), "images not covered by allowedRegistries: %s", strings.Join(bad, ", "))
	})

	It("T-12 GPU switch RBAC", func() {
		ctx := context.Background()
		role, err := clientset.RbacV1().ClusterRoles().Get(ctx, "gpu-modality-switcher", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			Skip("gpu-modality-switcher ClusterRole not present")
		}
		Expect(err).NotTo(HaveOccurred())
		var gpuNodes []string
		for _, rule := range role.Rules {
			if !hasString(rule.Resources, "nodes") || !hasString(rule.Verbs, "patch") {
				continue
			}
			gpuNodes = append(gpuNodes, rule.ResourceNames...)
		}
		var failures []string
		if len(gpuNodes) == 0 {
			failures = append(failures, "no resourceNames scoping — role can patch ANY node")
		}
		for _, node := range gpuNodes {
			allowed, err := sarAllowed(ctx, "patch", node)
			if err != nil {
				failures = append(failures, fmt.Sprintf("SubjectAccessReview for %s: %v", node, err))
				continue
			}
			if !allowed {
				failures = append(failures, fmt.Sprintf("cannot patch %s (role broken)", node))
			}
		}
		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		master := ""
		for _, node := range nodes.Items {
			if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
				master = node.Name
				break
			}
		}
		if master != "" {
			allowed, err := sarAllowed(ctx, "patch", master)
			if err != nil {
				failures = append(failures, fmt.Sprintf("SubjectAccessReview for master %s: %v", master, err))
			} else if allowed {
				failures = append(failures, fmt.Sprintf("can patch master %s — scoping has leaked", master))
			}
		}
		allowed, err := sarAllowed(ctx, "delete", "")
		if err != nil {
			failures = append(failures, fmt.Sprintf("SubjectAccessReview for delete: %v", err))
		} else if allowed {
			failures = append(failures, "can delete nodes")
		}
		vapGVR := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
		vap, err := dynamicClient.Resource(vapGVR).Get(ctx, "gpu-modality-switch-guard", metav1.GetOptions{})
		switch {
		case isMissing(err):
			failures = append(failures, "ValidatingAdmissionPolicy missing — RBAC alone cannot stop cordon/relabel")
		case err != nil:
			failures = append(failures, fmt.Sprintf("read ValidatingAdmissionPolicy: %v", err))
		default:
			vapbGVR := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}
			binding, err := dynamicClient.Resource(vapbGVR).Get(ctx, "gpu-modality-switch-guard", metav1.GetOptions{})
			switch {
			case isMissing(err):
				fmt.Printf("WARN admission policy binding missing; %s is not enforcing\n", vap.GetName())
			case err != nil:
				failures = append(failures, fmt.Sprintf("read ValidatingAdmissionPolicyBinding: %v", err))
			default:
				actions, _, _ := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
				if !hasString(actions, "Deny") {
					fmt.Printf("WARN admission policy present but NOT enforcing (%v)\n", actions)
				}
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-16 file integrity monitoring", func() {
		ctx := context.Background()
		var failures []string
		fiGVR := schema.GroupVersionResource{Group: "fileintegrity.openshift.io", Version: "v1alpha1", Resource: "fileintegrities"}
		fi, err := dynamicClient.Resource(fiGVR).Namespace("openshift-file-integrity").Get(ctx, "all-nodes", metav1.GetOptions{})
		if isMissing(err) {
			failures = append(failures, "no FileIntegrity all-nodes in openshift-file-integrity: no file integrity monitoring")
			Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
			return
		}
		Expect(err).NotTo(HaveOccurred())
		phase, _, _ := unstructured.NestedString(fi.Object, "status", "phase")
		if phase != "Active" {
			failures = append(failures, fmt.Sprintf("FileIntegrity all-nodes phase %q", phase))
		}
		statusGVR := schema.GroupVersionResource{Group: "fileintegrity.openshift.io", Version: "v1alpha1", Resource: "fileintegritynodestatuses"}
		statuses, err := dynamicClient.Resource(statusGVR).Namespace("openshift-file-integrity").List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		conditions := map[string]string{}
		for _, item := range statuses.Items {
			node, _, _ := unstructured.NestedString(item.Object, "nodeName")
			condition, _, _ := unstructured.NestedString(item.Object, "lastResult", "condition")
			conditions[node] = condition
		}
		for _, node := range nodeNames {
			if conditions[node] != "Succeeded" {
				failures = append(failures, fmt.Sprintf("%s AIDE condition %q", node, conditions[node]))
			}
		}
		ruleGVR := schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheusrules"}
		if _, err := dynamicClient.Resource(ruleGVR).Namespace("openshift-file-integrity").Get(ctx, "file-integrity", metav1.GetOptions{}); err != nil {
			failures = append(failures, fmt.Sprintf("PrometheusRule file-integrity missing: %v", err))
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("T-24 hub admission policies", func() {
		ctx := context.Background()
		crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
		if _, err := dynamicClient.Resource(crdGVR).Get(ctx, "hostedclusters.hypershift.openshift.io", metav1.GetOptions{}); isMissing(err) {
			Skip("not a hub (no HostedCluster API)")
		} else {
			Expect(err).NotTo(HaveOccurred())
		}
		vapbGVR := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}
		var failures []string
		for _, name := range []string{"compliant-hosted-clusters", "flashblade-nfs-tls"} {
			binding, err := dynamicClient.Resource(vapbGVR).Get(ctx, name, metav1.GetOptions{})
			if isMissing(err) {
				failures = append(failures, fmt.Sprintf("%s: binding missing", name))
				continue
			}
			Expect(err).NotTo(HaveOccurred())
			actions, _, _ := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
			if !hasString(actions, "Deny") {
				failures = append(failures, fmt.Sprintf("%s: %v (not enforcing)", name, actions))
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("C-01 GPU Operator ClusterPolicy ready", func() {
		ctx := context.Background()
		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		hasGPU := false
		for i := range nodes.Items {
			if nodeHasGPU(&nodes.Items[i]) {
				hasGPU = true
				break
			}
		}
		if !hasGPU {
			Skip("no GPU nodes")
		}

		gvr := schema.GroupVersionResource{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}
		list, err := dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{})
		var failures []string
		switch {
		case isMissing(err):
			failures = append(failures, "no ClusterPolicy API: GPU Operator is not installed")
		case err != nil:
			failures = append(failures, fmt.Sprintf("list ClusterPolicies: %v", err))
		case list == nil || len(list.Items) == 0:
			failures = append(failures, "no ClusterPolicy found")
		default:
			for _, item := range list.Items {
				state, _, _ := unstructured.NestedString(item.Object, "status", "state")
				if state != "ready" {
					failures = append(failures, fmt.Sprintf("ClusterPolicy %s state=%q", item.GetName(), state))
				} else {
					fmt.Printf("PASS ClusterPolicy %s ready\n", item.GetName())
				}
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("C-02 virtualization health", func() {
		ctx := context.Background()
		hcoGVR := schema.GroupVersionResource{Group: "hco.kubevirt.io", Version: "v1", Resource: "hyperconvergeds"}
		hco, err := dynamicClient.Resource(hcoGVR).Namespace("openshift-cnv").Get(ctx, "kubevirt-hyperconverged", metav1.GetOptions{})
		if isMissing(err) {
			Skip("no HyperConverged on this cluster")
		}
		Expect(err).NotTo(HaveOccurred())

		var failures []string
		available := conditionStatus(hco.Object, "Available")
		degraded := conditionStatus(hco.Object, "Degraded")
		if available != "True" || degraded == "True" {
			failures = append(failures, fmt.Sprintf("HyperConverged Available=%q Degraded=%q", available, degraded))
		} else {
			fmt.Println("PASS HyperConverged Available, not Degraded")
		}

		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		schedulable := 0
		for _, node := range nodes.Items {
			if node.Labels["kubevirt.io/schedulable"] == "true" {
				schedulable++
			}
		}
		if schedulable == 0 {
			failures = append(failures, "no node is kubevirt.io/schedulable")
		} else {
			fmt.Printf("PASS %d node(s) can run VMs\n", schedulable)
		}

		handlers := 0
		pods, err := clientset.CoreV1().Pods("openshift-cnv").List(ctx, metav1.ListOptions{
			LabelSelector: "kubevirt.io=virt-handler",
			FieldSelector: "status.phase=Running",
		})
		if err != nil {
			failures = append(failures, fmt.Sprintf("list virt-handler pods: %v", err))
		} else {
			for _, pod := range pods.Items {
				if pod.Status.Phase == corev1.PodRunning {
					handlers++
				}
			}
			if handlers < schedulable {
				failures = append(failures, fmt.Sprintf("virt-handler running on %d of %d VM-capable nodes", handlers, schedulable))
			} else {
				fmt.Printf("PASS virt-handler running on %d node(s)\n", handlers)
			}
		}
		if schedulable > 0 && schedulable < 2 {
			fmt.Println("WARN fewer than 2 VM-capable nodes: VMs cannot live migrate")
		}

		scs, err := clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		defaultSC := ""
		for _, sc := range scs.Items {
			if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
				sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true" {
				defaultSC = sc.Name
				break
			}
		}
		if defaultSC == "" {
			fmt.Println("WARN no default StorageClass")
		} else {
			spGVR := schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "storageprofiles"}
			profile, err := dynamicClient.Resource(spGVR).Get(ctx, defaultSC, metav1.GetOptions{})
			switch {
			case isMissing(err):
				fmt.Printf("WARN no CDI StorageProfile for default StorageClass %s\n", defaultSC)
			case err != nil:
				fmt.Printf("WARN read CDI StorageProfile %s: %v\n", defaultSC, err)
			default:
				sets, _, _ := unstructured.NestedSlice(profile.Object, "status", "claimPropertySets")
				rwx := false
				for _, raw := range sets {
					set, ok := raw.(map[string]interface{})
					if !ok {
						continue
					}
					modes, _, _ := unstructured.NestedStringSlice(set, "accessModes")
					if hasString(modes, "ReadWriteMany") {
						rwx = true
						break
					}
				}
				if rwx {
					fmt.Printf("PASS default StorageClass %s offers ReadWriteMany\n", defaultSC)
				} else {
					fmt.Printf("WARN default StorageClass %s has no ReadWriteMany: VMs on it cannot live migrate\n", defaultSC)
				}
			}
		}

		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("C-04 IOMMU and vfio on passthrough nodes", func() {
		ctx := context.Background()
		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		var passthrough []string
		for _, node := range nodes.Items {
			if node.Labels["nvidia.com/gpu.workload.config"] == "vm-passthrough" {
				passthrough = append(passthrough, node.Name)
			}
		}
		if len(passthrough) == 0 {
			Skip("no node currently in vm-passthrough modality")
		}

		var failures []string
		for _, node := range passthrough {
			if err, ok := probeFailure(node); ok {
				failures = append(failures, fmt.Sprintf("%s: %s", node, err))
				continue
			}
			before := len(failures)
			v := values(probeLogs[node])
			if v["VFIO_IOMMU_TYPE1"] != "loaded" {
				failures = append(failures, fmt.Sprintf("%s vfio_iommu_type1=%s", node, v["VFIO_IOMMU_TYPE1"]))
			}
			groups, _ := strconv.Atoi(v["IOMMU_GROUPS"])
			if groups <= 0 {
				failures = append(failures, fmt.Sprintf("%s has no IOMMU groups", node))
			}
			if off := v["IOMMU_OFF"]; off != "" {
				failures = append(failures, fmt.Sprintf("%s is in vm-passthrough mode with %s", node, off))
			}
			if len(failures) == before {
				fmt.Printf("PASS %s vfio_iommu_type1 loaded, %d IOMMU groups\n", node, groups)
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("C-03 nodes are Ready", func() {
		ctx := context.Background()
		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		var failures []string
		for _, node := range nodes.Items {
			ready := false
			for _, condition := range node.Status.Conditions {
				if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
					ready = true
					break
				}
			}
			if !ready {
				failures = append(failures, fmt.Sprintf("%s is not Ready", node.Name))
			}
			if node.Spec.Unschedulable {
				fmt.Printf("WARN %s is cordoned\n", node.Name)
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("C-05 GPU VM tenancy and configuration", func() {
		ctx := context.Background()
		hcoGVR := schema.GroupVersionResource{Group: "hco.kubevirt.io", Version: "v1", Resource: "hyperconvergeds"}
		hco, err := dynamicClient.Resource(hcoGVR).Namespace("openshift-cnv").Get(ctx, "kubevirt-hyperconverged", metav1.GetOptions{})
		if isMissing(err) {
			Skip("no HyperConverged on this cluster")
		}
		Expect(err).NotTo(HaveOccurred())

		kvGVR := schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}
		kv, err := dynamicClient.Resource(kvGVR).Namespace("openshift-cnv").Get(ctx, "kubevirt-kubevirt-hyperconverged", metav1.GetOptions{})
		if err != nil {
			fmt.Printf("WARN read KubeVirt CR: %v\n", err)
			kv = nil
		}

		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())

		vmGVR := schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
		vmiGVR := schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
		vmList, err := dynamicClient.Resource(vmGVR).List(ctx, metav1.ListOptions{})
		if err != nil && !isMissing(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		vmiList, err := dynamicClient.Resource(vmiGVR).List(ctx, metav1.ListOptions{})
		if err != nil && !isMissing(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		var failures []string
		pass := func(format string, args ...interface{}) { fmt.Printf("PASS "+format+"\n", args...) }
		warn := func(format string, args ...interface{}) { fmt.Printf("WARN "+format+"\n", args...) }

		byNode := map[string]map[string]bool{}
		if vmiList != nil {
			for _, item := range vmiList.Items {
				if len(gpuDeviceNames(virtualMachineTestSpec(item.Object, "VirtualMachineInstance"))) == 0 {
					continue
				}
				phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
				if phase == "Succeeded" || phase == "Failed" {
					continue
				}
				node, _, _ := unstructured.NestedString(item.Object, "status", "nodeName")
				if node == "" {
					continue
				}
				if byNode[node] == nil {
					byNode[node] = map[string]bool{}
				}
				byNode[node][item.GetNamespace()] = true
			}
		}
		if len(byNode) == 0 {
			pass("tenancy: no GPU VM running")
		} else {
			for node, namespaces := range byNode {
				if len(namespaces) > 1 {
					var names []string
					for namespace := range namespaces {
						names = append(names, namespace)
					}
					sort.Strings(names)
					failures = append(failures, fmt.Sprintf("tenancy: %s runs GPU VMs from %d namespaces: %s", node, len(names), strings.Join(names, ", ")))
				} else {
					for namespace := range namespaces {
						pass("tenancy: %s runs GPU VMs from one namespace (%s)", node, namespace)
					}
				}
			}
		}

		evictionDefault := ""
		if virtualization, _, _ := unstructured.NestedMap(hco.Object, "spec", "virtualization"); virtualization != nil {
			evictionDefault, _, _ = unstructured.NestedString(virtualization, "evictionStrategy")
		}
		if evictionDefault == "" {
			evictionDefault, _, _ = unstructured.NestedString(hco.Object, "spec", "evictionStrategy")
		}
		if evictionDefault == "" {
			evictionDefault = "LiveMigrate"
		}

		evictionChecked := 0
		if vmList != nil {
			for _, item := range vmList.Items {
				spec := virtualMachineTestSpec(item.Object, "VirtualMachine")
				if len(gpuDeviceNames(spec)) == 0 {
					continue
				}
				evictionChecked++
				name := item.GetNamespace() + "/" + item.GetName()
				own, found, _ := unstructured.NestedString(spec, "evictionStrategy")
				effective := evictionDefault
				where := "from the HyperConverged"
				if found && own != "" {
					effective = own
					where = "own"
				}
				if effective == "LiveMigrate" {
					failures = append(failures, fmt.Sprintf("eviction: GPU VM %s: evictionStrategy LiveMigrate (%s) blocks every drain", name, where))
				} else {
					pass("eviction: GPU VM %s: evictionStrategy %s (%s)", name, effective, where)
				}

				volumes, _, _ := unstructured.NestedSlice(spec, "volumes")
				var containerDisks []string
				for _, raw := range volumes {
					volume, ok := raw.(map[string]interface{})
					if !ok {
						continue
					}
					if _, found := volume["containerDisk"]; found {
						containerDisks = append(containerDisks, fmt.Sprintf("%v", volume["name"]))
					}
				}
				if len(containerDisks) > 0 {
					warn("disk: GPU VM %s boots from containerDisk %s; use a DataVolume or retest", name, strings.Join(containerDisks, ", "))
				} else {
					pass("disk: GPU VM %s uses no containerDisk", name)
				}
			}
		}
		if evictionChecked == 0 {
			pass("eviction: no GPU VM defined")
			pass("disk: no GPU VM defined")
		}

		advertised := map[string][]string{}
		for _, node := range nodes.Items {
			if node.Labels["nvidia.com/gpu.workload.config"] != "vm-passthrough" {
				continue
			}
			for name, quantity := range node.Status.Allocatable {
				resource := string(name)
				if strings.HasPrefix(resource, "nvidia.com/") && resource != "nvidia.com/gpu" && quantity.Value() > 0 {
					advertised[resource] = append(advertised[resource], node.Name)
				}
			}
		}
		if len(advertised) == 0 {
			pass("permitted: no node advertises a passthrough GPU")
		} else {
			hcoSpec, _, _ := unstructured.NestedMap(hco.Object, "spec")
			hcoPermitted := permittedHostDevices(hcoSpec)
			kvPermitted := map[string]bool{}
			if kv != nil {
				kvSpec, _, _ := unstructured.NestedMap(kv.Object, "spec")
				kvPermitted = permittedHostDevices(kvSpec)
			}
			for resource, nodeNames := range advertised {
				var missing, internal []string
				if _, ok := hcoPermitted[resource]; !ok {
					missing = append(missing, "HyperConverged")
				} else if !hcoPermitted[resource] {
					internal = append(internal, "HyperConverged")
				}
				if _, ok := kvPermitted[resource]; !ok {
					missing = append(missing, "KubeVirt")
				} else if !kvPermitted[resource] {
					internal = append(internal, "KubeVirt")
				}
				switch {
				case len(missing) > 0:
					failures = append(failures, fmt.Sprintf("permitted: %s (on %s) not in permittedHostDevices of %s", resource, strings.Join(nodeNames, ", "), strings.Join(missing, " and ")))
				case len(internal) > 0:
					failures = append(failures, fmt.Sprintf("permitted: %s has externalResourceProvider false on %s", resource, strings.Join(internal, " and ")))
				default:
					pass("permitted: %s permitted on the HyperConverged and KubeVirt", resource)
				}
			}
		}

		var vmNodes []string
		for _, node := range nodes.Items {
			if strings.HasPrefix(node.Labels["nvidia.com/gpu.workload.config"], "vm-") {
				vmNodes = append(vmNodes, node.Name)
			}
		}
		policyGVR := schema.GroupVersionResource{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}
		policies, err := dynamicClient.Resource(policyGVR).List(ctx, metav1.ListOptions{})
		if err != nil && !isMissing(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		if policies == nil || len(policies.Items) == 0 {
			if len(vmNodes) > 0 {
				failures = append(failures, fmt.Sprintf("sandbox: no ClusterPolicy, but %s labelled for VMs", strings.Join(vmNodes, ", ")))
			}
		} else {
			sandbox, _, _ := unstructured.NestedMap(policies.Items[0].Object, "spec", "sandboxWorkloads")
			enabled := false
			if sandbox != nil {
				enabled, _, _ = unstructured.NestedBool(sandbox, "enabled")
			}
			switch {
			case enabled:
				pass("sandbox: sandboxWorkloads enabled")
			case len(vmNodes) > 0:
				failures = append(failures, fmt.Sprintf("sandbox: sandboxWorkloads off, so the VM label on %s does nothing", strings.Join(vmNodes, ", ")))
			default:
				pass("sandbox: sandboxWorkloads off, and no node labelled for VMs")
			}
			if enabled {
				defaultWorkload, found, _ := unstructured.NestedString(sandbox, "defaultWorkload")
				if !found || defaultWorkload == "" {
					defaultWorkload = "container"
				}
				if defaultWorkload == "container" {
					pass("sandbox: defaultWorkload container")
				} else {
					warn("sandbox: defaultWorkload %s: every unlabelled GPU node serves VMs", defaultWorkload)
				}
			}
		}

		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("C-06 one default StorageClass", func() {
		ctx := context.Background()
		scs, err := clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		var defaults []string
		for _, sc := range scs.Items {
			if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
				sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true" {
				defaults = append(defaults, sc.Name)
			}
		}
		if len(defaults) == 0 {
			Fail("no default StorageClass")
		}
		if len(defaults) > 1 {
			fmt.Printf("WARN multiple default StorageClasses: %s\n", strings.Join(defaults, ", "))
		} else {
			fmt.Printf("PASS default StorageClass %s\n", defaults[0])
		}
	})

	It("C-07 namespaces have a default-deny NetworkPolicy", func() {
		ctx := context.Background()
		namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		var failures []string
		for _, namespace := range namespaces.Items {
			if namespace.Status.Phase == corev1.NamespaceTerminating {
				continue
			}
			if networkPolicyExempt != nil && networkPolicyExempt.MatchString(namespace.Name) {
				continue
			}
			policies, err := clientset.NetworkingV1().NetworkPolicies(namespace.Name).List(ctx, metav1.ListOptions{})
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s list NetworkPolicies: %v", namespace.Name, err))
				continue
			}
			found := false
			for _, policy := range policies.Items {
				selector := policy.Spec.PodSelector
				if len(selector.MatchLabels) > 0 || len(selector.MatchExpressions) > 0 {
					continue
				}
				hasIngress, hasEgress := false, false
				for _, policyType := range policy.Spec.PolicyTypes {
					switch string(policyType) {
					case "Ingress":
						hasIngress = true
					case "Egress":
						hasEgress = true
					}
				}
				if hasIngress && hasEgress && len(policy.Spec.Ingress) == 0 && len(policy.Spec.Egress) == 0 {
					found = true
					break
				}
			}
			if !found {
				failures = append(failures, fmt.Sprintf("%s has no default-deny NetworkPolicy", namespace.Name))
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	It("C-08 GPU memory-clearing record matches passthrough nodes", func() {
		ctx := context.Background()
		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		var passthrough []corev1.Node
		for _, node := range nodes.Items {
			if node.Labels["nvidia.com/gpu.workload.config"] == "vm-passthrough" {
				passthrough = append(passthrough, node)
			}
		}
		if len(passthrough) == 0 {
			Skip("no node currently in vm-passthrough modality")
		}
		if len(memclearRecord) == 0 {
			fmt.Println("WARN no memory-clearing record configured; set uat_compliance.memclearRecord")
			return
		}
		expectedKernel := memclearRecord["kernel"]
		expectedGPU := strings.TrimSpace(memclearRecord["device"] + " " + memclearRecord["resetMethod"])
		if expectedKernel == "" || expectedGPU == "" {
			fmt.Println("WARN memory-clearing record needs kernel, device and resetMethod")
			return
		}
		for _, node := range passthrough {
			if err, ok := probeFailure(node.Name); ok {
				fmt.Printf("WARN %s: %s\n", node.Name, err)
				continue
			}
			kernel := node.Status.NodeInfo.KernelVersion
			raw := linesWithPrefix(probeLogs[node.Name], "GPU=")
			gpus := make([]string, 0, len(raw))
			for _, line := range raw {
				gpus = append(gpus, strings.TrimPrefix(line, "GPU="))
			}
			sort.Strings(gpus)
			unique := gpus[:0]
			for _, gpu := range gpus {
				if len(unique) == 0 || unique[len(unique)-1] != gpu {
					unique = append(unique, gpu)
				}
			}
			switch {
			case kernel != expectedKernel:
				fmt.Printf("WARN %s kernel %s differs from the memory-clearing record %s\n", node.Name, kernel, expectedKernel)
			case strings.Join(unique, "\n") != expectedGPU:
				fmt.Printf("WARN %s GPUs %q differ from the memory-clearing record %q\n", node.Name, strings.Join(unique, "; "), expectedGPU)
			default:
				fmt.Printf("PASS %s matches the memory-clearing record (kernel, device, reset method)\n", node.Name)
			}
			if memclearRecord["vbios"] == "" {
				fmt.Printf("WARN %s GPU firmware is not in the memory-clearing record\n", node.Name)
			}
		}
	})

	It("C-09 unrequested GPU protections", func() {
		ctx := context.Background()
		policyGVR := schema.GroupVersionResource{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}
		policies, err := dynamicClient.Resource(policyGVR).List(ctx, metav1.ListOptions{})
		if isMissing(err) {
			Skip("no GPU Operator ClusterPolicy")
		}
		Expect(err).NotTo(HaveOccurred())
		if policies == nil || len(policies.Items) == 0 {
			Skip("no GPU Operator ClusterPolicy")
		}

		var failures []string
		toolkit, _, _ := unstructured.NestedSlice(policies.Items[0].Object, "spec", "toolkit", "env")
		setting := false
		for _, raw := range toolkit {
			entry, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			name, _, _ := unstructured.NestedString(entry, "name")
			value, _, _ := unstructured.NestedString(entry, "value")
			if name == "ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED" && value == "false" {
				setting = true
				break
			}
		}
		if !setting {
			failures = append(failures, "toolkit does not set ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED=false")
		} else {
			fmt.Println("PASS toolkit refuses unrequested NVIDIA_VISIBLE_DEVICES")
		}

		vapGVR := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
		if _, err := dynamicClient.Resource(vapGVR).Get(ctx, "nvidia-runtimeclass-operator-only", metav1.GetOptions{}); isMissing(err) {
			fmt.Println("WARN admission policy nvidia-runtimeclass-operator-only is missing")
		} else if err != nil {
			failures = append(failures, fmt.Sprintf("read admission policy: %v", err))
		} else {
			vapbGVR := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}
			binding, err := dynamicClient.Resource(vapbGVR).Get(ctx, "nvidia-runtimeclass-operator-only", metav1.GetOptions{})
			if isMissing(err) {
				fmt.Println("WARN admission policy binding nvidia-runtimeclass-operator-only is missing")
			} else if err != nil {
				failures = append(failures, fmt.Sprintf("read admission policy binding: %v", err))
			} else {
				actions, _, _ := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
				if !hasString(actions, "Deny") {
					fmt.Printf("WARN admission policy nvidia-runtimeclass-operator-only is not enforcing (%v)\n", actions)
				} else {
					fmt.Println("PASS nvidia* RuntimeClasses are restricted to the GPU Operator")
				}
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})
})

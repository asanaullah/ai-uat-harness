// Assisted by Claude Opus
package test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	clientset  *kubernetes.Clientset
	adminNS    string
	probeImage string
	probeSA    string
	probeLogs  map[string]string
	probeErrs  map[string]string
	nodeNames  []string
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
	clientset, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())

	ctx := context.Background()
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
})

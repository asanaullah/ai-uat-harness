// Assisted by Claude Opus
package test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func resourceMustParse(value string) resource.Quantity {
	return resource.MustParse(value)
}

func resourcePtr(value string) *resource.Quantity {
	quantity := resource.MustParse(value)
	return &quantity
}

func boolPtr(value bool) *bool {
	return &value
}

var (
	fipsClientset *kubernetes.Clientset
	fipsAdminNS   string
	fipsProbeSA   string
)

func TestFIPSImage(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "FIPS Image Test Suite")
}

var _ = BeforeSuite(func() {
	fipsAdminNS = os.Getenv("UAT_ADMIN_NAMESPACE")
	if fipsAdminNS == "" {
		fipsAdminNS = "uat-admin"
	}
	fipsProbeSA = os.Getenv("NODE_PROBE_SERVICE_ACCOUNT")
	if fipsProbeSA == "" {
		fipsProbeSA = "uat-admin-sa"
	}
	config, err := rest.InClusterConfig()
	Expect(err).NotTo(HaveOccurred())
	fipsClientset, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
})

const fipsScanScript = `set -u
export HOME=/work GOTOOLCHAIN=auto GOPATH=/work/go GOCACHE=/work/cache GOFLAGS="-mod=mod -p=2" GOMAXPROCS=2 PATH=/work/go/bin:$PATH
go install github.com/openshift/check-payload@latest >/work/build.log 2>&1 || { tail -5 /work/build.log; exit 1; }
report() {
  grep -v -E '^\s*$|^\+[-+]+\+$|^\| EXECUTABLE NAME|using embedded config|"scan" version|can.t find rpmdb|scanning failed|command failed|/etc/redhat-release .*could not find distribution' /work/out.txt | tail -n 40
}
if [ "$MODE" = control ]; then
  mkdir -p /work/src && cd /work/src
  printf 'package main\nimport ("crypto/sha256";"fmt")\nfunc main(){ fmt.Println(sha256.Sum256([]byte("x"))) }\n' > main.go
  go mod init control >/dev/null 2>&1; go mod edit -go=1.26.0
  for kind in redhat upstream upstream-fips140; do
    R=/work/root-$kind; mkdir -p $R/usr/bin $R/etc; cp /etc/redhat-release $R/etc/
    case $kind in
      redhat)           CGO_ENABLED=0 go build -o $R/usr/bin/control . ;;
      upstream)         CGO_ENABLED=0 GOTOOLCHAIN=go1.26.0 go build -o $R/usr/bin/control . ;;
      upstream-fips140) CGO_ENABLED=0 GOTOOLCHAIN=go1.26.0 GOFIPS140=v1.0.0 go build -o $R/usr/bin/control . ;;
    esac || { echo "build $kind failed"; continue; }
    echo "################ control-$kind"
    check-payload scan local --path $R > /work/out.txt 2>&1; echo "exit=$?"; report
  done
else
  go install github.com/google/go-containerregistry/cmd/crane@latest >>/work/build.log 2>&1 || { tail -5 /work/build.log; exit 1; }
  n=0
  for img in $IMAGES; do
    n=$((n+1)); IMG=/work/img$n; mkdir -p $IMG
    echo "################ $img"
    INSECURE=""
    case "$img" in
      image-registry.openshift-image-registry.svc:5000/*)
        crane auth login image-registry.openshift-image-registry.svc:5000 -u scan \
          -p "$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" --insecure >/dev/null 2>&1
        INSECURE=--insecure ;;
    esac
    crane export $INSECURE --platform linux/amd64 "$img" - 2>/work/crane.err \
      | tar --no-same-owner --no-same-permissions --warning=no-unknown-keyword -x -C $IMG 2>/dev/null
    if [ -z "$(ls -A $IMG)" ]; then echo "could not pull: $(tail -1 /work/crane.err)"; continue; fi
    check-payload scan local --path $IMG > /work/out.txt 2>&1; echo "exit=$?"; report
    chmod -R u+w $IMG 2>/dev/null; rm -rf $IMG
  done
fi
echo "== done"`

var _ = Describe("FIPS image scan", Label("pass-fail"), func() {
	It("scans images for FIPS-capable binaries", func() {
		ctx := context.Background()
		images := strings.TrimSpace(os.Getenv("FIPS_IMAGES"))
		mode := "images"
		if images == "" {
			mode = "control"
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "uat-fips-scan-",
				Namespace:    fipsAdminNS,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "uat-harness",
					"uat/test":                     "fips-image",
				},
				Annotations: map[string]string{
					"openshift.io/required-scc": "restricted-v2",
				},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: fipsProbeSA,
				RestartPolicy:      corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:    "scan",
					Image:   "registry.access.redhat.com/ubi9/go-toolset:latest",
					Command: []string{"/bin/bash", "-c", fipsScanScript},
					Env: []corev1.EnvVar{
						{Name: "MODE", Value: mode},
						{Name: "IMAGES", Value: images},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resourceMustParse("2"),
							corev1.ResourceMemory: resourceMustParse("4Gi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resourceMustParse("8Gi"),
						},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: "/work"}},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: boolPtr(false),
						RunAsNonRoot:             boolPtr(true),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
				}},
				Volumes: []corev1.Volume{{
					Name: "work",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
						SizeLimit: resourcePtr("40Gi"),
					}},
				}},
			},
		}

		created, err := fipsClientset.CoreV1().Pods(fipsAdminNS).Create(ctx, pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			_ = fipsClientset.CoreV1().Pods(fipsAdminNS).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
		}()

		deadline := time.Now().Add(29 * time.Minute)
		phase := corev1.PodUnknown
		for {
			current, err := fipsClientset.CoreV1().Pods(fipsAdminNS).Get(ctx, created.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			phase = current.Status.Phase
			if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
				break
			}
			if time.Now().After(deadline) {
				Fail(fmt.Sprintf("FIPS scan pod did not finish: phase=%s", phase))
			}
			time.Sleep(5 * time.Second)
		}

		logs, err := fipsClientset.CoreV1().Pods(fipsAdminNS).GetLogs(created.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		Expect(err).NotTo(HaveOccurred())
		output := string(logs)
		fmt.Println(output)
		Expect(phase).To(Equal(corev1.PodSucceeded), "FIPS scan pod failed")
		if strings.Contains(output, "could not pull:") {
			Fail("one or more FIPS scan images could not be pulled")
		}
		if strings.Contains(output, "build ") && strings.Contains(output, " failed") {
			Fail("one or more FIPS scanner control builds failed")
		}
	})
})

// Assisted by Claude Opus
package test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
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
	tlsClientset *kubernetes.Clientset
	tlsAdminNS   string
	tlsProbeSA   string
)

func TestTLSTraffic(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "TLS Traffic Test Suite")
}

var _ = BeforeSuite(func() {
	tlsAdminNS = os.Getenv("UAT_ADMIN_NAMESPACE")
	if tlsAdminNS == "" {
		tlsAdminNS = "uat-admin"
	}
	tlsProbeSA = os.Getenv("NODE_PROBE_SERVICE_ACCOUNT")
	if tlsProbeSA == "" {
		tlsProbeSA = "uat-admin-sa"
	}
	config, err := rest.InClusterConfig()
	Expect(err).NotTo(HaveOccurred())
	tlsClientset, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
})

type tlsMaterial struct {
	caPEM         []byte
	serverCertPEM []byte
	serverKeyPEM  []byte
	clientCert    tls.Certificate
	roots         *x509.CertPool
}

func newTLSMaterial() (*tlsMaterial, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "uat-tls-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "etcd-test"},
		DNSNames:     []string{"etcd-test"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "uat-tls-test-client"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}

	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return nil, err
	}
	clientKeyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		return nil, err
	}
	clientCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER}),
	)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return &tlsMaterial{
		caPEM:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		serverCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		serverKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}),
		clientCert:    clientCert,
		roots:         roots,
	}, nil
}

func handshake(addr string, config *tls.Config) error {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, config)
	if err != nil {
		return err
	}
	return conn.Close()
}

var _ = Describe("TLS traffic", Label("pass-fail"), func() {
	It("runs a handshake burst against a test etcd", func() {
		ctx := context.Background()
		etcdPods, err := tlsClientset.CoreV1().Pods("openshift-etcd").List(ctx, metav1.ListOptions{LabelSelector: "app=etcd"})
		if err != nil || etcdPods == nil || len(etcdPods.Items) == 0 {
			Skip("no etcd pods on this cluster (hosted control plane or no local etcd)")
		}
		etcdPod := etcdPods.Items[0]
		if len(etcdPod.Spec.Containers) == 0 {
			Skip("etcd pod has no containers")
		}
		etcdContainer := etcdPod.Spec.Containers[0]
		for _, container := range etcdPod.Spec.Containers {
			if container.Name == "etcd" {
				etcdContainer = container
				break
			}
		}
		etcdImage := etcdContainer.Image
		cipherSuites := ""
		for _, env := range etcdContainer.Env {
			if env.Name == "ETCD_CIPHER_SUITES" {
				cipherSuites = env.Value
			}
		}

		material, err := newTLSMaterial()
		Expect(err).NotTo(HaveOccurred())

		zero := int64(0)
		initScript := `set -eu
mkdir -p /certs
printf '%s' "$CA_B64" | base64 -d > /certs/ca.crt
printf '%s' "$SERVER_CERT_B64" | base64 -d > /certs/server.crt
printf '%s' "$SERVER_KEY_B64" | base64 -d > /certs/server.key
chmod 0644 /certs/*`

		etcdEnv := []corev1.EnvVar{}
		if cipherSuites != "" {
			etcdEnv = append(etcdEnv, corev1.EnvVar{Name: "ETCD_CIPHER_SUITES", Value: cipherSuites})
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "uat-tls-traffic-",
				Namespace:    tlsAdminNS,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "uat-harness",
					"uat/test":                     "tls-traffic",
				},
				Annotations: map[string]string{
					"openshift.io/required-scc": "privileged",
				},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: tlsProbeSA,
				RestartPolicy:      corev1.RestartPolicyNever,
				InitContainers: []corev1.Container{{
					Name:    "certs",
					Image:   "registry.access.redhat.com/ubi9/ubi:9.8",
					Command: []string{"/bin/sh", "-c", initScript},
					Env: []corev1.EnvVar{
						{Name: "CA_B64", Value: base64.StdEncoding.EncodeToString(material.caPEM)},
						{Name: "SERVER_CERT_B64", Value: base64.StdEncoding.EncodeToString(material.serverCertPEM)},
						{Name: "SERVER_KEY_B64", Value: base64.StdEncoding.EncodeToString(material.serverKeyPEM)},
					},
					SecurityContext: &corev1.SecurityContext{RunAsUser: &zero},
					VolumeMounts:    []corev1.VolumeMount{{Name: "certs", MountPath: "/certs"}},
				}},
				Containers: []corev1.Container{{
					Name:    "etcd",
					Image:   etcdImage,
					Command: []string{"etcd"},
					Args: []string{
						"--name=etcd-test",
						"--data-dir=/data",
						"--listen-client-urls=https://0.0.0.0:2379",
						"--advertise-client-urls=https://etcd-test:2379",
						"--cert-file=/certs/server.crt",
						"--key-file=/certs/server.key",
						"--trusted-ca-file=/certs/ca.crt",
						"--client-cert-auth=true",
					},
					Env:   etcdEnv,
					Ports: []corev1.ContainerPort{{Name: "client", ContainerPort: 2379}},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "certs", MountPath: "/certs"},
						{Name: "data", MountPath: "/data"},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "certs", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
		}
		created, err := tlsClientset.CoreV1().Pods(tlsAdminNS).Create(ctx, pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			_ = tlsClientset.CoreV1().Pods(tlsAdminNS).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
		}()

		deadline := time.Now().Add(3 * time.Minute)
		ip := ""
		lastPhase := corev1.PodUnknown
		for {
			current, err := tlsClientset.CoreV1().Pods(tlsAdminNS).Get(ctx, created.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			lastPhase = current.Status.Phase
			if current.Status.Phase == corev1.PodFailed {
				Fail(fmt.Sprintf("test etcd pod failed before serving: %s", current.Status.Message))
			}
			if current.Status.Phase == corev1.PodRunning && current.Status.PodIP != "" {
				ip = current.Status.PodIP
				break
			}
			if time.Now().After(deadline) {
				Fail(fmt.Sprintf("test etcd pod did not run: phase=%s", lastPhase))
			}
			time.Sleep(2 * time.Second)
		}
		address := net.JoinHostPort(ip, "2379")
		config := &tls.Config{
			RootCAs:      material.roots,
			Certificates: []tls.Certificate{material.clientCert},
			ServerName:   "etcd-test",
			MinVersion:   tls.VersionTLS12,
		}

		readyDeadline := time.Now().Add(60 * time.Second)
		var readyErr error
		for {
			if readyErr = handshake(address, config); readyErr == nil {
				break
			}
			if time.Now().After(readyDeadline) {
				Fail(fmt.Sprintf("test etcd did not accept TLS handshakes: %v", readyErr))
			}
			time.Sleep(2 * time.Second)
		}

		sequentialFailures := 0
		var sequentialErr error
		for i := 0; i < 200; i++ {
			if err := handshake(address, config); err != nil {
				sequentialFailures++
				sequentialErr = err
			}
		}

		const burst = 1000
		burstFailures := 0
		var burstErr error
		var lock sync.Mutex
		var wait sync.WaitGroup
		for i := 0; i < burst; i++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				if err := handshake(address, config); err != nil {
					lock.Lock()
					burstFailures++
					burstErr = err
					lock.Unlock()
				}
			}()
		}
		wait.Wait()

		fmt.Printf("TLS handshakes: sequential=%d failures=%d burst=%d failures=%d\n",
			200, sequentialFailures, burst, burstFailures)
		if sequentialFailures > 0 {
			fmt.Printf("sequential error: %v\n", sequentialErr)
		}
		if burstFailures > 0 {
			fmt.Printf("burst error: %v\n", burstErr)
		}

		current, err := tlsClientset.CoreV1().Pods(tlsAdminNS).Get(ctx, created.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(current.Status.Phase).To(Equal(corev1.PodRunning), "test etcd did not survive the handshake burst")
		Expect(sequentialFailures).To(Equal(0), "sequential handshake failures")
		Expect(burstFailures).To(Equal(0), "burst handshake failures")
	})
})

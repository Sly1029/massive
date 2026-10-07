// Package miniotest starts the repository's source-pinned MinIO image for
// functional S3 tests. Build it first with scripts/build-minio-test-image.sh.
package miniotest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	AccessKey = "massive-test-access"
	SecretKey = "massive-test-secret"
)

// Start runs a disposable MinIO container and returns its host:port. It skips
// the test when Docker or the pinned image is unavailable.
func Start(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable; skipping real MinIO test: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("could not allocate a local port for MinIO; skipping real MinIO test: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	_, source, _, _ := runtime.Caller(0)
	image, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "..", "conformance", "minio", "image-reference"))
	if err != nil {
		t.Fatal(err)
	}
	container := "massive-minio-" + strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())
	output, err := exec.Command("docker", "run", "-d", "--rm",
		"--name", container,
		"-p", fmt.Sprintf("127.0.0.1:%d:9000", port),
		"-e", "MINIO_ROOT_USER="+AccessKey,
		"-e", "MINIO_ROOT_PASSWORD="+SecretKey,
		strings.TrimSpace(string(image)), "server", "/data",
	).CombinedOutput()
	if err != nil {
		t.Skipf("could not start MinIO container with docker; skipping real MinIO test: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", container).Run()
	})

	endpoint := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", endpoint, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return endpoint
		}
		time.Sleep(500 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
	t.Skipf("MinIO container did not become ready; skipping real MinIO test\n%s", logs)
	return ""
}

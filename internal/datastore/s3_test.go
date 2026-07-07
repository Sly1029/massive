package datastore

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	minioAccessKey = "massive-test-access"
	minioSecretKey = "massive-test-secret"
)

func TestS3DatastoreContract(t *testing.T) {
	endpoint := startMinIO(t)
	t.Setenv("MASSIVE_TEST_S3_ACCESS_KEY", minioAccessKey)
	t.Setenv("MASSIVE_TEST_S3_SECRET_KEY", minioSecretKey)

	RunDatastoreContract(t, func(t *testing.T) Datastore {
		t.Helper()

		store, err := NewS3Datastore(context.Background(), S3Config{
			Endpoint:           endpoint,
			Bucket:             "massive-datastore-contract",
			Region:             "us-east-1",
			Prefix:             strings.ToLower(t.Name()),
			AccessKeyEnv:       "MASSIVE_TEST_S3_ACCESS_KEY",
			SecretAccessKeyEnv: "MASSIVE_TEST_S3_SECRET_KEY",
			CreateBucket:       true,
		})
		if err != nil {
			t.Fatalf("new s3 datastore: %v", err)
		}
		return store
	})
}

func startMinIO(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable; skipping real MinIO datastore contract: %v", err)
	}

	port, err := freeTCPPort()
	if err != nil {
		t.Skipf("could not allocate a local port for MinIO; skipping real MinIO datastore contract: %v", err)
	}
	container := "massive-minio-" + strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())
	args := []string{
		"run", "-d", "--rm",
		"--name", container,
		"-p", fmt.Sprintf("127.0.0.1:%d:9000", port),
		"-e", "MINIO_ROOT_USER=" + minioAccessKey,
		"-e", "MINIO_ROOT_PASSWORD=" + minioSecretKey,
		"minio/minio", "server", "/data",
	}
	output, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Skipf("could not start MinIO container with docker; skipping real MinIO datastore contract: %v\n%s", err, output)
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
	t.Skipf("MinIO container did not become ready; skipping real MinIO datastore contract\n%s", logs)
	return ""
}

func freeTCPPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate TCP port: %w", err)
	}
	defer listener.Close()

	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected TCP address type: %T", listener.Addr())
	}
	return address.Port, nil
}

func TestNewS3DatastoreRequiresCredentialsFromEnv(t *testing.T) {
	t.Setenv("MASSIVE_TEST_MISSING_ACCESS_KEY", "")
	t.Setenv("MASSIVE_TEST_MISSING_SECRET_KEY", "")

	_, err := NewS3Datastore(context.Background(), S3Config{
		Endpoint:           "127.0.0.1:9000",
		Bucket:             "bucket",
		AccessKeyEnv:       "MASSIVE_TEST_MISSING_ACCESS_KEY",
		SecretAccessKeyEnv: "MASSIVE_TEST_MISSING_SECRET_KEY",
	})
	if err == nil {
		t.Fatal("expected missing credential error")
	}
	if !strings.Contains(err.Error(), "MASSIVE_TEST_MISSING_ACCESS_KEY") {
		t.Fatalf("error = %v, want env var name", err)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

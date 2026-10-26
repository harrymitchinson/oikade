//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harrymitchinson/oikade/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = envOr("E2E_IMG", "example.com/oikade:v0.0.1")
	// managerVersion is baked into the manager image, and every role logs it on startup.
	managerVersion = envOr("E2E_VERSION", "v0.0.0-e2e")
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind.
//
// To enable kubectl kuberc (use custom kubectl configurations), set: KUBECTL_KUBERC=true
// By default, kuberc is disabled to ensure consistent test behavior across different environments.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting oikade e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// make test-e2e builds the image while Kind creates the cluster.
	if os.Getenv("E2E_IMAGE_BUILT") != "true" {
		By("building the manager image")
		cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage),
			fmt.Sprintf("VERSION=%s", managerVersion))
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")
	}

	// TODO(user): If you want to change the e2e test vendor from Kind,
	// ensure the image is built and available, then remove the following block.
	By("loading the manager image on Kind")
	err := utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	configureKubectlKubeRC()
	waitForLastRunsNamespaces()
})

// envOr returns the environment variable key, or def if it's unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Disable kubectl kuberc by default for test isolation.
// This prevents local kubectl configurations from affecting test behavior.
// To enable kuberc, set: KUBECTL_KUBERC=true
func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}

// waitForLastRunsNamespaces waits until the namespaces a previous run deleted on a
// reused cluster are gone, so this run can create them again.
func waitForLastRunsNamespaces() {
	By("waiting for the last run's namespaces to be deleted")
	cmd := exec.Command("kubectl", "wait", "--for=delete", "--timeout=3m",
		"ns/"+namespace, "ns/"+homeNamespace, "ns/"+readmeNamespace)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "The last run's namespaces are still there")
}

//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/willhaben/pingdom-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "pingdom-operator"

// serviceAccountName created for the project
const serviceAccountName = "pingdom-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "pingdom-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "pingdom-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("creating the Pingdom API token Secret")
		// A dummy token is enough for the manager to start; only the real-API test needs a valid one.
		token := pingdomToken()
		if token == "" {
			token = "dummy"
		}
		tokenFile := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(tokenFile, []byte(token), 0o600)).To(Succeed())
		cmd = exec.Command("kubectl", "create", "secret", "generic", "pingdom-controller-manager",
			"-n", namespace, "--from-file=pingdom-api-token="+tokenFile)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the Pingdom token Secret")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the pod's status")
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=pingdom-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		It("should create, update and delete a check in Pingdom", func() {
			token := pingdomToken()
			if token == "" {
				Skip("PINGDOM_API_TOKEN is not set and there is no .env file")
			}

			name := fmt.Sprintf("e2e-check-%d", time.Now().Unix())
			manifest := func(name string, resolution int) string {
				// paused so the test never triggers real alerts
				return fmt.Sprintf(`apiVersion: pingdom.willhaben.at.willhaben.at/v1alpha1
kind: Check
metadata:
  name: %s
  namespace: %s
spec:
  host: example.com
  type: http
  url: /
  encryption: true
  port: 443
  paused: true
  resolution: %d
  tags: [pingdom-operator-e2e]
`, name, namespace, resolution)
			}
			kubectlApply := func(m string) {
				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(m)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
			}
			jsonpath := func(g Gomega, name, path string) string {
				cmd := exec.Command("kubectl", "get", "checks.pingdom.willhaben.at.willhaben.at", name,
					"-n", namespace, "-o", "jsonpath="+path)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				return strings.TrimSpace(out)
			}
			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "checks.pingdom.willhaben.at.willhaben.at", name,
					"-n", namespace, "--ignore-not-found", "--timeout=2m")
				_, _ = utils.Run(cmd)
			})

			By("creating the Check")
			kubectlApply(manifest(name, 5))
			var checkID string
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, name, "{.status.conditions[?(@.type=='Ready')].status}")).To(Equal("True"),
					"Ready condition; message: "+jsonpath(g, name, "{.status.conditions[?(@.type=='Ready')].message}"))
				checkID = jsonpath(g, name, "{.status.checkID}")
				g.Expect(checkID).NotTo(BeEmpty())
			}).Should(Succeed())

			By("verifying the check exists in Pingdom")
			code, body := pingdomGet(token, checkID)
			Expect(code).To(Equal(http.StatusOK))
			Expect(body["check"].(map[string]any)["name"]).To(Equal(name))
			Expect(body["check"].(map[string]any)["resolution"]).To(BeEquivalentTo(5))

			By("updating the Check")
			kubectlApply(manifest(name, 15))
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, name, "{.status.observedGeneration}")).To(Equal(jsonpath(g, name, "{.metadata.generation}")))
				_, body := pingdomGet(token, checkID)
				g.Expect(body["check"].(map[string]any)["resolution"]).To(BeEquivalentTo(15))
			}).Should(Succeed())

			By("deleting the Check")
			cmd := exec.Command("kubectl", "delete", "checks.pingdom.willhaben.at.willhaben.at", name,
				"-n", namespace, "--timeout=2m")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			code, _ = pingdomGet(token, checkID)
			// Pingdom answers 403 "Invalid check identifier" for missing checks
			Expect(code).To(BeElementOf(http.StatusForbidden, http.StatusNotFound))

			By("deleting a Check whose Pingdom check is already gone")
			gone := name + "-gone"
			kubectlApply(manifest(gone, 5))
			var goneID string
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, gone, "{.status.conditions[?(@.type=='Ready')].status}")).To(Equal("True"))
				goneID = jsonpath(g, gone, "{.status.checkID}")
				g.Expect(goneID).NotTo(BeEmpty())
			}).Should(Succeed())
			code, _ = pingdomDo(http.MethodDelete, token, goneID)
			Expect(code).To(Equal(http.StatusOK))
			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "checks.pingdom.willhaben.at.willhaben.at", gone,
					"-n", namespace, "--ignore-not-found", "--timeout=2m")
				_, _ = utils.Run(cmd)
			})
			cmd = exec.Command("kubectl", "delete", "checks.pingdom.willhaben.at.willhaben.at", gone,
				"-n", namespace, "--timeout=2m")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "deletion must finish even though Pingdom no longer has the check")

			By("verifying no orphaned (duplicate) checks were left in Pingdom")
			for _, n := range []string{name, gone} {
				Eventually(func() int { return pingdomCount(token, n) }, 30*time.Second).Should(BeZero(),
					"Pingdom still has a check named %s that no Check resource tracks", n)
			}
		})

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// pingdomToken returns PINGDOM_API_TOKEN, falling back to the (gitignored) .env file at the repo root,
// which may hold the bare token or PINGDOM_API_TOKEN=<token>.
func pingdomToken() string {
	if t := os.Getenv("PINGDOM_API_TOKEN"); t != "" {
		return t
	}
	dir, err := utils.GetProjectDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "PINGDOM_API_TOKEN=")
}

// pingdomGet fetches a check straight from the Pingdom API.
func pingdomGet(token, checkID string) (int, map[string]any) {
	return pingdomDo(http.MethodGet, token, checkID)
}

func pingdomDo(method, token, checkID string) (int, map[string]any) {
	req, err := http.NewRequest(method, "https://api.pingdom.com/api/3.1/checks/"+checkID, nil)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// pingdomCount returns how many checks named name exist in Pingdom.
func pingdomCount(token, name string) int {
	req, err := http.NewRequest(http.MethodGet, "https://api.pingdom.com/api/3.1/checks?include_tags=true", nil)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
	n := 0
	for _, c := range body.Checks {
		if c.Name == name {
			n++
		}
	}
	return n
}
